package fs

import (
	"fmt"
	"github.com/fsnotify/fsnotify"
	"io/ioutil"
	//"log"
	"../fm"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// something to think about: see if it's possible to have some structure here that could help avoid
// creating multiple watchers for a single folder

type folder struct {
	path     string
	contents map[string]os.FileInfo
	m        sync.Mutex
	watchers []chan bool
	done     chan struct{}
	uid      uint64
	count    uint64
}

type mutexFolders struct {
	f map[string]*folder
	m sync.Mutex
}

var folders mutexFolders = mutexFolders{f: make(map[string]*folder)}

//get the folder struct
func GetFolder(path string) (*folder, error) {
	fmt.Println(path)
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fmt.Println(absPath)
	folders.m.Lock()
	defer folders.m.Unlock()

	if f, ok := folders.f[absPath]; ok {
		fmt.Println("already watched")
		return f, nil
	} else {
		f := &folder{path: absPath}
		err := f.Refresh()
		if err != nil {
			return nil, err
		}
		f.done = make(chan struct{})
		//f.watchers = make([]chan bool)

		folders.f[absPath] = f

		return f, nil
	}

}

// Get folder path
func (f *folder) Path() string {
	return f.path
}

// Get folder contents
func (f *folder) Contents() map[string]fm.File {
	//if folder not being watched, refresh it
	if f.count == 0 {
		f.Refresh()
	}
	f.m.Lock()
	defer f.m.Unlock()

	m := make(map[string]fm.File)

	for fileName, fileInfo := range f.contents {
		m[fileName] = fileInfo
	}

	return m
}

// Get channel for notifications on changes to the folder
func (f *folder) Watch() chan bool {
	f.m.Lock()
	if f.count == 0 {
		go f.fsWatcher()
	}
	f.m.Unlock()
	w := make(chan bool)
	f.watchers = append(f.watchers, w)
	f.count++
	return w
}

func (f *folder) Close() {
	fmt.Println(f.count)
	if f.count <= 1 {
		fmt.Println("close")
		close(f.done)
		folders.m.Lock()
		delete(folders.f, f.path)
		folders.m.Unlock()
	} else {
		f.count--
	}
}

func (f *folder) notifyWatchers() {
	for _, w := range f.watchers {
		w <- true
	}
}
func (f *folder) closeWatchers() {
	for _, w := range f.watchers {
		close(w)
	}
}

func (f *folder) fsWatcher() {
	fmt.Println("making a watcher")
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	f.Refresh() //have the latest data before starting to delta over it with the watcher

	defer watcher.Close()
	defer f.closeWatchers()

	err = watcher.Add(f.path)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	if f.path != filepath.Dir(f.path) {
		err = watcher.Add(filepath.Dir(f.path))
		if err != nil {
			fmt.Println("error:", err)
			return
		}
	}

	folderRenamed := false

	for {
		select {
		case event := <-watcher.Events:
			//since we are watching the parent as well (most likely), check path.
			if filepath.Dir(event.Name) == f.path {
				switch event.Op {
				//create or modify - update map
				case fsnotify.Create:
					fallthrough
				case fsnotify.Rename:
					fallthrough
				case fsnotify.Chmod:
					fallthrough
				case fsnotify.Write:
					f.updateItem(event.Name)

				//deletion - remove from map
				case fsnotify.Remove:
					f.removeItem(event.Name)
				}
				f.notifyWatchers()
				//fmt.Println("(w): "+event.Name+" - ", event)
			} else if event.Name == f.path && event.Op == fsnotify.Rename {
				folderRenamed = true

				//this is... quite a thing to do.
				go func() {
					time.Sleep(10 * time.Millisecond)
					if folderRenamed == true {
						fmt.Println("folder gone")
						f.cleanup()
					}
				}()
			} else if folderRenamed == true && event.Op == fsnotify.Create {
				//compare folder uids
				if f.uid != getPathUID(event.Name) {
					f.cleanup()
					continue
				}

				//if this is indeed the folder we were in, change this folder object
				fmt.Println("caught new folder")
				absPath, err := filepath.Abs(event.Name)
				if err != nil {
					fmt.Println("error:", err)
					return
				}
				//stop watching old path
				err = watcher.Remove(f.path)
				if err != nil {
					fmt.Println("error:", err)
					return
				}

				// move folder to the new path in the map
				folders.m.Lock()
				delete(folders.f, f.path)
				folders.f[absPath] = f
				folders.m.Unlock()

				f.path = absPath
				f.Refresh()
				//start watching new path
				err = watcher.Add(f.path)
				if err != nil {
					fmt.Println("error:", err)
					return
				}

				folderRenamed = false
			}

			//fmt.Println(f.path, "(w) -", event.Name, event.Op)
		case err := <-watcher.Errors:
			fmt.Println("error:", err)
		case <-f.done:
			fmt.Println("watcher over")
			return
		}
	}
}

func (f *folder) cleanup() {
	if f.path == "" && f.contents == nil {
		return
	}
	f.path = ""
	f.contents = nil
	f.Close()
}

// Get initial folder contents from file system
// Might be nice to have a mechanism that would just go ahead and refresh if
// many update/remove events are queued.
func (f *folder) Refresh() error {
	f.m.Lock()
	defer f.m.Unlock()
	// replace the map
	f.contents = make(map[string]os.FileInfo)

	files, err := ioutil.ReadDir(f.path)
	if err != nil {
		return err
	}

	for _, file := range files {
		f.contents[file.Name()] = file
		//fmt.Println(file.Name())
	}

	f.uid = getPathUID(f.path)
	return nil
}

//unix only
//figure out a drop-in uid function for other os?
func getPathUID(path string) uint64 {
	fileinfo, _ := os.Stat(path)
	stat, ok := fileinfo.Sys().(*syscall.Stat_t)
	if !ok {
		// 0 in inodes indicates an error, so...
		return 0
	}

	return stat.Ino
}

// Takes an absolute path to file and stats it
// Also functions as an add function
func (f *folder) updateItem(path string) {
	f.m.Lock()
	defer f.m.Unlock()
	file, err := os.Stat(path)

	if err != nil {
		fmt.Println("error:", err)
		return
	}
	f.contents[filepath.Base(path)] = file
	//fmt.Printf("up: %s\n", f.contents[filepath.Base(path)].Name())
}

func (f *folder) removeItem(path string) {
	f.m.Lock()
	defer f.m.Unlock()
	delete(f.contents, filepath.Base(path))
}

// moving files is tricky. fsnotify does not link "rename" and "create" events, which means that it is possible to misinterpret these events in a situation where a lot of events happen in a folder.
// what do?
