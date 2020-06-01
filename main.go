package main

import (
	"github.com/fsnotify/fsnotify"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TODO:
// create dir path
// -> create path/.keep
// -> commit path/.keep
// create file path
// -> commit path
// modify file
// -> commit path
// remove file
// remove dir

var watcher *fsnotify.Watcher
var repo, _ = filepath.Abs("./watchable")

func main() {
	watcher, _ = fsnotify.NewWatcher()
	defer watcher.Close()

	log.Println(repo)
	if err := filepath.Walk(repo, watchDir); err != nil {
		log.Println("Error:", err)
	}

	done := make(chan bool)

	go func() {
		for {
			select {
			case <-done:
				log.Println("Watcher done")
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Create == fsnotify.Create {
					addDirWatcher(event.Name)
				} else {
					commitAndPush()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}

				log.Println("Error:", err)
			}
		}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	tickerDone := make(chan bool)
	for {
		select {
		case <-tickerDone:
			log.Println("Timer done")
			return
		case <-ticker.C:
			pull()
		}
	}
}

func addDirWatcher(name string) error {
	fi, err := os.Stat(name)

	if err != nil {
		// some temp files are removed when the change handler runs (e.g.
		// from emacs), so we ignore files that throw an error here ...
		return nil
	}

	return watchDir(name, fi, nil)
}

func watchDir(path string, fi os.FileInfo, err error) error {
	if path == ".git" {
		return nil
	}

	if fi.Mode().IsDir() && !strings.HasPrefix(path, repo+"/.git") {
		log.Println("Watching " + path)
		touchKeepFile(path)
		commitAndPush()
		return watcher.Add(path)
	}

	return nil
}

func touchKeepFile(path string) error {
	if path == repo {
		return nil
	}

	fullPath := filepath.Join(path, ".keep")
	_, err := os.Stat(fullPath)
	if os.IsNotExist(err) {
		file, err := os.Create(fullPath)

		if err != nil {
			return err
		}

		defer file.Close()
	}

	return nil
}

func repoClean() bool {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = repo
	cmd.Stderr = os.Stderr

	if out, err := cmd.Output(); err != nil || string(out) == "" {
		return true
	}

	return false
}

func commitAndPush() error {
	if repoClean() {
		return nil
	}

	add()
	commit()
	push()

	return nil
}

func add() {
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = repo
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	_, err := cmd.Output()
	if err != nil {
		log.Printf("%s: error adding changes: %s\n%s", repo, err, err.(*exec.ExitError).Stderr)
	} else {
		log.Printf("%s: added changes", repo)
	}
}

func commit() {
	cmd := exec.Command("git", "commit", "-m", "Auto-commit from autogit")
	cmd.Dir = repo
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	_, err := cmd.Output()
	if err != nil {
		log.Printf("%s: error committing: %s\n%s", repo, err, err.(*exec.ExitError).Stderr)
	} else {
		log.Printf("%s: committed changes", repo)
	}
}

func push() {
	cmd := exec.Command("git", "push")
	cmd.Dir = repo
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	_, err := cmd.Output()
	if err != nil {
		log.Printf("%s: error pushing: %s\n%s", repo, err, err.(*exec.ExitError).Stderr)
	} else {
		log.Printf("%s: pushed changes", repo)
	}
}

func pull() {
	cmd := exec.Command("git", "pull", "--ff-only")
	cmd.Dir = repo

	_, err := cmd.Output()
	if err != nil {
		log.Printf("%s: error pulling: %s\n%s", repo, err, err.(*exec.ExitError).Stderr)
	}
}
