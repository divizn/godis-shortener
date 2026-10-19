package main

import (
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/divizn/godis/client"
)

func TestLifecycleOnGodisCluster(t *testing.T) {
	addrs := os.Getenv("GODIS_ADDRS")
	if addrs == "" {
		t.Skip("GODIS_ADDRS not set")
	}
	c, err := client.Dial(strings.Split(addrs, ",")...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a, err := newApp(config{baseURL: "http://localhost", createLimit: 1000}, c, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	a.cfg.baseURL = srv.URL
	a.base, _ = url.Parse(srv.URL)
	lifecycle(t, srv.URL)
}
