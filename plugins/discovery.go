package plugins

import (
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const defaultLocalRegistry = "/usr/share/docker/plugins"

type Registry interface {
	Plugins() ([]Plugin, error)
}

type LocalRegistry struct {
	path string
}

func newLocalRegistry(path string) *LocalRegistry {
	if len(path) == 0 {
		path = defaultLocalRegistry
	}

	return &LocalRegistry{path}
}

func (l *LocalRegistry) Plugins() ([]Plugin, error) {
	var plugins []Plugin

	err := filepath.Walk(l.path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		p, err := readPluginInfo(path, info)
		if err == nil {
			plugins = append(plugins, p)
		}
		return err
	})

	return plugins, err
}

func readPluginInfo(path string, fi os.FileInfo) (Plugin, error) {
	name := strings.Split(fi.Name(), ".")[0]

	if fi.Mode()&os.ModeSocket != 0 {
		return &RemotePlugin{
			Name: name,
			Addr: "unix://" + path,
		}, nil
	}

	content, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	addr := strings.TrimSpace(string(content))

	u, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}

	if len(u.Scheme) == 0 {
		return nil, fmt.Errorf("Unknown protocol")
	}

	return &RemotePlugin{
		Name: name,
		Addr: addr,
	}, nil
}
