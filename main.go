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

func main() {
	watcher, _ = fsnotify.NewWatcher()
	defer watcher.Close()

	if err := filepath.Walk("./watchable", watchDir); err != nil {
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
		log.Println("Error:", err)
	}

	return watchDir(name, fi, nil)
}

// watchDir gets run as a walk func, searching for directories to add watchers to
func watchDir(path string, fi os.FileInfo, err error) error {
	if fi.Mode().IsDir() {
		return watcher.Add(path)
	}

	return nil
}
