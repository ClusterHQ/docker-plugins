package plugins

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

var (
	mux    *http.ServeMux
	client *http.Client
	server *httptest.Server
)

func setupRemotePluginServer() string {
	mux = http.NewServeMux()
	server = httptest.NewServer(mux)
	return server.URL
}

func teardownRemotePluginServer() {
	if server != nil {
		server.Close()
	}
}

func TestFailedActivation(t *testing.T) {
	addr := setupRemotePluginServer()
	defer teardownRemotePluginServer()

	r := &RemotePlugin{"echo", addr}
	if _, err := r.Activate(); err == nil {
		t.Fatal("Expected error, was nil")
	}
}

func TestMissingExtensions(t *testing.T) {
	addr := setupRemotePluginServer()
	defer teardownRemotePluginServer()

	mux.HandleFunc("/Plugin.Activate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatal("Expected POST, got %s\n", r.Method)
		}

		header := w.Header()
		header.Set("Content-Type", "application/json")

		w.Write([]byte("{}"))
	})

	r := &RemotePlugin{"echo", addr}
	if _, err := r.Activate(); err == nil {
		t.Fatal("Expected no extensions error, was nil")
	}
}

func TestActivateGoodManifest(t *testing.T) {
	addr := setupRemotePluginServer()
	defer teardownRemotePluginServer()

	mux.HandleFunc("/Plugin.Activate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatal("Expected POST, got %s\n", r.Method)
		}

		header := w.Header()
		header.Set("Content-Type", "application/json")

		m := Manifest{[]string{"volume", "network"}}
		b, _ := json.Marshal(m)
		w.Write(b)
	})

	r := &RemotePlugin{"echo", addr}
	m, err := r.Activate()
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(m.Extensions, []string{"volume", "network"}) {
		t.Fatalf("Expected %v, was %v\n", []string{"volume", "network"}, m.Extensions)
	}
}
