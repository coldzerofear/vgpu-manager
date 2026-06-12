package main

import (
	"os"
	"os/signal"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"
)

func NewFSWatcher(files ...string) (*fsnotify.Watcher, error) {
	klog.V(3).Info("Starting FS watcher.")
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	for _, f := range files {
		err = watcher.Add(f)
		if err != nil {
			watcher.Close()
			return nil, err
		}
	}

	return watcher, nil
}

func NewOSWatcher(sigs ...os.Signal) chan os.Signal {
	klog.V(3).Info("Starting OS watcher.")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, sigs...)

	return sigChan
}
