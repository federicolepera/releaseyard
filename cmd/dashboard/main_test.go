/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestNewSessionStoreMemory(t *testing.T) {
	t.Setenv("SESSION_STORE", "")
	store, closeStore, err := newSessionStore()
	if err != nil {
		t.Fatalf("memory store: %v", err)
	}
	if store != nil {
		t.Fatal("memory store should let the dashboard create its own in-memory store")
	}
	closeStore()
}

func TestNewSessionStoreRedis(t *testing.T) {
	server := miniredis.RunT(t)
	t.Setenv("SESSION_STORE", "redis")
	t.Setenv("REDIS_ADDR", server.Addr())
	t.Setenv("REDIS_DB", "2")
	t.Setenv("REDIS_TLS_ENABLED", "false")

	store, closeStore, err := newSessionStore()
	if err != nil {
		t.Fatalf("redis store: %v", err)
	}
	if store == nil {
		t.Fatal("redis store is nil")
	}
	closeStore()
}

func TestNewSessionStoreRejectsInvalidConfiguration(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unknown type":    {"SESSION_STORE": "memcached"},
		"missing address": {"SESSION_STORE": "redis", "REDIS_ADDR": ""},
		"invalid db":      {"SESSION_STORE": "redis", "REDIS_ADDR": "127.0.0.1:6379", "REDIS_DB": "nope"},
	} {
		t.Run(name, func(t *testing.T) {
			for key, value := range env {
				t.Setenv(key, value)
			}
			if _, _, err := newSessionStore(); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}
