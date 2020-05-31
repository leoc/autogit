package main

import (
	"fmt"
	"github.com/fsnotify/fsnotify"
	"log"
	"os"
	"path/filepath"
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

	//
	done := make(chan bool)

	//
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Create == fsnotify.Create {
					addDirWatcher(event.Name)
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					log.Println("modified file:", event.Name)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}

				log.Println("Error:", err)
			}
		}
	}()

	<-done
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

