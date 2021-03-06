package worker

import (
	"fmt"

	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/qfarm/qfarm"
	"github.com/qfarm/qfarm/redis"
)

type Worker struct {
	linter   *Metalinter
	redis    *redis.Service
	notifier *Notifier
	coverage *CoverageChecker
	config   *Cfg
}

func NewWorker(config *Cfg) (*Worker, error) {
	w := &Worker{config: config}
	cfg := redis.NewConfig().WithConnection(config.RedisConn).WithPassword(config.RedisPass)
	var err error
	w.redis, err = redis.NewService(cfg)
	if err != nil {
		return nil, fmt.Errorf("Can't create the redis service: %v\n", err)
	}

	w.notifier = NewNotifier(w.redis)
	w.linter = NewMetalinter(config, w.redis, w.notifier)
	w.coverage = NewCoverageChecker(config, w.notifier)

	return w, nil
}

func (w *Worker) Run() error {
	if err := w.redis.Subscribe("test-q-channel", w.fetchAndAnalyze); err != nil {
		return err
	}

	return nil
}

func (w *Worker) fetchAndAnalyze(data interface{}) error {
	elem, err := w.redis.ListPop("test-q-list") // TODO: drain list to the bottom
	if err != nil {
		// do nothing other worker might got the value from list before
		return nil
	}

	if err := w.analyze(string(elem.([]byte))); err != nil {
		w.notifier.SendEvent(string(elem.([]byte)), fmt.Sprintf("Error: %s", err.Error()), EventTypeError)
		log.Printf("Error during worker analysis! Err: %v \n", err)
	}

	return nil
}

func (w *Worker) analyze(repo string) error {
	start := time.Now()

	// download repo
	if err := w.download(repo); err != nil {
		return err
	}

	if err := w.markAsUserRepo(repo); err != nil {
		return err
	}

	lastCommitHash, err := lastCommitHash(repo)
	if err != nil {
		return err
	}

	log.Printf("Hash of last commit %s", lastCommitHash)

	// get last build number
	firstTimeBuild := false
	buildInfo, err := w.getLastBuildInfo(repo)
	if err != nil {
		if err == redis.ErrNotFound {
			firstTimeBuild = true
		} else {
			return err
		}
	}

	if !firstTimeBuild && w.config.CheckLastCommitHash {
		// someone wants to analyze the same repo twice
		if buildInfo.CommitHash == lastCommitHash {
			w.notifier.SendEventWithPayload(repo, fmt.Sprintf("Repo %s already analyzed!", repo), EventTypeAlreadyAnalyzed, fmt.Sprintf("%s", buildInfo.No))
			return fmt.Errorf("repo %s already analyzed!", repo)
		}
	}

	// generate new build no
	newBuild := qfarm.Build{Repo: repo, CommitHash: lastCommitHash, Time: time.Now().UTC()}
	if firstTimeBuild {
		newBuild.No = 1
	} else {
		newBuild.No = buildInfo.No + 1
	}

	// create repo config
	buildCfg, err := LoadRepoCfg(repo, path.Join(os.Getenv("GOPATH"), "src", repo))
	if err != nil {
		return err
	}
	newBuild.Config = *buildCfg

	// generate directory structure
	ft, err := BuildTree(buildCfg.Path)
	if err != nil {
		return err
	}

	// run all linters
	if err := w.linter.Start(*buildCfg, newBuild.No, ft); err != nil {
		return err
	}

	// run coverage
	if err := w.coverage.Start(*buildCfg, ft); err != nil {
		return err
	}

	if err := w.storeNodes(buildCfg.Repo, newBuild.No, ft); err != nil {
		return fmt.Errorf("can't store nodes in Redis: %v", err)
	}

	root, ok := ft.FilesMap[ft.Root]
	if !ok {
		return fmt.Errorf("Can't find root!")
	}

	// generate report
	r := qfarm.Report{
		Repo:              newBuild.Repo,
		No:                newBuild.No,
		Score:             calculateScore(root),
		Time:              qfarm.JSONTime(start),
		Took:              time.Now().Sub(start).String(),
		CommitHash:        newBuild.CommitHash,
		Config:            newBuild.Config,
		Coverage:          root.Coverage,
		TestsNo:           root.TestsNo,
		FailedNo:          root.FailedNo,
		PassedNo:          root.PassedNo,
		IssuesNo:          root.IssuesNo,
		ErrorsNo:          root.ErrorsNo,
		WarningsNo:        root.WarningsNo,
		TechnicalDeptCost: root.WarningsNo*CostOfWarning + root.ErrorsNo*CostOfError,
		TechnicalDeptTime: (time.Duration(root.ErrorsNo*FixTimeOfError)*time.Minute + time.Duration(root.WarningsNo*FixTimeOfWarning)*time.Minute).String(),
	}

	// store report in redis
	rData, err := json.Marshal(r)
	if err != nil {
		return err
	}

	// add new build to global list of all builds
	if err := w.redis.ListPush("all-builds", rData); err != nil {
		return err
	}

	// add new build to list of builds per repo
	if err := w.redis.ListPush("builds:"+repo, rData); err != nil {
		return err
	}

	if err := w.redis.Set(fmt.Sprintf("reports:%s:%d", newBuild.Repo, newBuild.No), -1, rData); err != nil {
		return err
	}

	w.notifier.SendEventWithPayload(repo, "All tasks done!", EventTypeAllDone, fmt.Sprintf("%d", newBuild.No))

	fmt.Printf("All done\n")
	return nil
}

const (
	CostOfWarning = 10
	CostOfError   = 14

	FixTimeOfWarning = 15
	FixTimeOfError   = 20
)

func calculateScore(n *qfarm.Node) int {
	// max penalty for coverage is 50%
	coveragePenalty := int(0.5 * (float64(100) - n.Coverage))

	// calculate issues penalty
	issuesPenalty := int(float64(n.ErrorsNo)*0.3 + float64(n.WarningsNo)*0.15)

	// normalize output
	if issuesPenalty > 50 {
		issuesPenalty = 50
	}

	// calculate score
	score := 100 - coveragePenalty - issuesPenalty

	if score < 0 {
		return 0
	}
	return score
}

func (w *Worker) storeNodes(repo string, no int, ft *FilesMap) error {
	prefix := "files:" + repo
	for path, node := range ft.FilesMap {
		n := strings.Index(node.Path, repo)
		if n < 0 {
			return fmt.Errorf("can't trim node path: %s", node.Path)
		}
		localPath := node.Path[:n]
		node.Path = strings.TrimPrefix(node.Path, localPath)
		node.ParentPath = strings.TrimPrefix(node.ParentPath, localPath)
		for i := range node.Nodes {
			node.Nodes[i].Path = strings.TrimPrefix(node.Nodes[i].Path, localPath)
			node.Nodes[i].ParentPath = strings.TrimPrefix(node.Nodes[i].ParentPath, localPath)
		}
		if node.Dir {
			path += "/"
		}
		p := strings.TrimPrefix(path, ft.Root)
		key := fmt.Sprintf("%s:%d:%s", prefix, no, string(p))
		data, err := json.Marshal(node)
		if err != nil {
			return fmt.Errorf("can't marshal node %+v: %v", node, err)
		}
		if err := w.redis.Set(key, -1, string(data)); err != nil {
			return fmt.Errorf("can't set in Redis: %v", err)
		}
	}
	return nil
}

func (w *Worker) getLastBuildInfo(repo string) (qfarm.Report, error) {
	var build qfarm.Report
	data, err := w.redis.ListGetLast("builds:" + repo)
	if err != nil {
		return build, err
	}

	if err := json.Unmarshal(data.([]byte), &build); err != nil {
		return build, err
	}

	return build, nil
}

func (w *Worker) download(repo string) error {
	fmt.Printf("Downloading %s...\n", repo)
	if err := exec.Command("go", "get", "-u", "-t", path.Join(repo, "...")).Run(); err != nil {
		return err
	}

	fmt.Printf("Repo %s downloaded!\n", repo)

	w.notifier.SendEvent(repo, fmt.Sprintf("Repo %s downloaded", repo), EventTypeDownloadDone)

	return nil
}

func (w *Worker) markAsUserRepo(repo string) error {
	userName := strings.Split(repo, "/")[1]
	_, err := w.redis.SortedSetRank("users:"+userName+":repos", repo)

	return err
}

func lastCommitHash(repo string) (string, error) {
	repoPath := path.Join(os.Getenv("GOPATH"), "src", repo)

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return strings.TrimSuffix(string(out), "\n"), nil
}
