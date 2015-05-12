package plugins

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUnknownLocalPath(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "docker-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	l := newLocalRegistry(filepath.Join(tmpdir, "unknown"))
	_, err = l.Plugins()
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("Expected error for unknown directory")
	}
}

func TestLocalSocket(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "docker-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)
	l, err := net.Listen("unix", filepath.Join(tmpdir, "echo.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	r := newLocalRegistry(tmpdir)
	plugins, err := r.Plugins()
	if err != nil {
		t.Fatal(err)
	}

	if len(plugins) != 1 {
		t.Fatalf("Expected 1 plugin registered, got %d\n", len(plugins))
	}

	p := plugins[0]

	pp, err := r.Plugin("echo")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, pp) {
		t.Fatalf("Expected %v, was %v\n", p, pp)
	}

	if p.Name != "echo" {
		t.Fatalf("Expected plugin `echo`, got %s\n", p.Name)
	}

	addr := fmt.Sprintf("unix://%s/echo.sock", tmpdir)
	if p.Addr != addr {
		t.Fatalf("Expected plugin addr `%s`, got %s\n", addr, p.Addr)
	}
}

func TestFileSpecPlugin(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "docker-test")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		name string
		addr string
		fail bool
	}{
		{filepath.Join(tmpdir, "echo.spec"), "echo", "unix://var/lib/docker/plugins/echo.sock", false},
		{filepath.Join(tmpdir, "foo.spec"), "foo", "tcp://localhost:8080", false},
		//{filepath.Join(tmpdir, "foo/foo.spec"), "foo", "tcp://localhost:8080", false},
		{filepath.Join(tmpdir, "bar.spec"), "bar", "localhost:8080", true}, // unknown transport
	}

	for _, c := range cases {
		if err = os.MkdirAll(path.Dir(c.path), 0755); err != nil {
			t.Fatal(err)
		}
		if err = ioutil.WriteFile(c.path, []byte(c.addr), 0644); err != nil {
			t.Fatal(err)
		}

		r := newLocalRegistry(tmpdir)
		plugins, err := r.Plugins()
		if c.fail && err == nil {
			continue
		}

		if err != nil {
			t.Fatal(err)
		}

		if len(plugins) != 1 {
			t.Fatalf("Expected 1 plugin registered, got %d\n", len(plugins))
		}

		p := plugins[0]

		pp, err := r.Plugin(p.Name)
		if err != nil {
			t.Fatal(err, p.Name)
		}
		if !reflect.DeepEqual(p, pp) {
			t.Fatalf("Expected %v, was %v\n", p, pp)
		}

		if p.Name != c.name {
			t.Fatalf("Expected plugin `%s`, got %s\n", c.name, p.Name)
		}

		if p.Addr != c.addr {
			t.Fatalf("Expected plugin addr `%s`, got %s\n", c.addr, p.Addr)
		}
		os.Remove(c.path)
	}
}
