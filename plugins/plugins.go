package plugins

import (
	"errors"
	"sync"

	"github.com/Sirupsen/logrus"
)

var (
	ErrNotImplements = errors.New("Plugin not implements particular driver")
)

type plugins struct {
	sync.Mutex
	plugins map[string]*Plugin
}

var storage = plugins{plugins: make(map[string]*Plugin)}

type Manifest struct {
	Implements []string
}

type Plugin struct {
	Name     string
	Addr     string
	Client   *Client
	Manifest *Manifest
}

func (p *Plugin) activate() error {
	m := new(Manifest)
	p.Client = NewClient(p.Addr)
	err := p.Client.Call("Plugin.Activate", nil, m)
	if err != nil {
		return err
	}
	logrus.Errorf("Manifest: %v", m)
	p.Manifest = m
	return nil
}

func load(name string) (*Plugin, error) {
	registry := newLocalRegistry("")
	pl, err := registry.Plugin(name)
	if err != nil {
		return nil, err
	}
	if err := pl.activate(); err != nil {
		return nil, err
	}
	return pl, nil
}

func get(name string) (*Plugin, error) {
	storage.Lock()
	defer storage.Unlock()
	pl, ok := storage.plugins[name]
	if ok {
		return pl, nil
	}
	pl, err := load(name)
	if err != nil {
		return nil, err
	}
	logrus.Errorf("Plugin: %v", pl)
	storage.plugins[name] = pl
	return pl, nil
}

func Get(name, imp string) (*Plugin, error) {
	pl, err := get(name)
	if err != nil {
		return nil, err
	}
	for _, driver := range pl.Manifest.Implements {
		logrus.Errorf("Implements: %s", driver)
		if driver == imp {
			return pl, nil
		}
	}
	return nil, ErrNotImplements
}
