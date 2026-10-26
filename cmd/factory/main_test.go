package main

import "testing"

func TestListenAddr(t *testing.T) {
	if got := listenAddr("", 7478); got != "127.0.0.1:7478" {
		t.Errorf("default = %q", got)
	}
	if got := listenAddr("box:0", 7478); got != "box:0" {
		t.Errorf("-listen = %q", got)
	}
}
