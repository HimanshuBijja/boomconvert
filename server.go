package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"boomconvert/adapters"

	"github.com/gorilla/websocket"
)

type Server struct {
	store     *Store
	tools     *ToolHealth
	registry  *adapters.Registry
	watcher   *Watcher
	hub       *Hub
	staticFS  fs.FS
	upgrader  websocket.Upgrader
	onFolders func()
}

func NewServer(store *Store, tools *ToolHealth, reg *adapters.Registry, w *Watcher, hub *Hub, staticFS fs.FS, onFolders func()) *Server {
	return &Server{
		store:    store,
		tools:    tools,
		registry: reg,
		watcher:  w,
		hub:      hub,
		staticFS: staticFS,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// Local-only server; same-origin checks are unnecessary.
				return true
			},
		},
		onFolders: onFolders,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(s.staticFS)))
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/status/toggle", s.handleToggle)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/tools/refresh", s.handleToolsRefresh)
	mux.HandleFunc("/api/folders", s.handleFolders)
	mux.HandleFunc("/api/folders/", s.handleFolderItem)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/matrix", s.handleMatrix)
	mux.HandleFunc("/api/analytics", s.handleAnalytics)
	mux.HandleFunc("/ws", s.handleWS)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"paused": s.watcher.IsPaused(),
	})
}

func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.watcher.SetPaused(!s.watcher.IsPaused())
	writeJSON(w, 200, map[string]bool{"paused": s.watcher.IsPaused()})
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.tools.Snapshot())
}

func (s *Server) handleToolsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.tools.Probe(r.Context())
	s.hub.Broadcast(Event{Type: "tool_health_changed", Payload: s.tools.Snapshot()})
	writeJSON(w, 200, s.tools.Snapshot())
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		folders, err := s.store.ListFolders()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 200, folders)
	case http.MethodPost:
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		body.Path = strings.TrimSpace(body.Path)
		if body.Path == "" {
			http.Error(w, "path required", 400)
			return
		}
		wf, err := s.store.AddFolder(body.Path)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if s.onFolders != nil {
			s.onFolders()
		}
		writeJSON(w, 200, wf)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) handleFolderItem(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/folders/")
	parts := strings.Split(tail, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "id required", 400)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := s.store.RemoveFolder(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if s.onFolders != nil {
			s.onFolders()
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if len(parts) == 2 && parts[1] == "toggle" && r.Method == http.MethodPost {
		folders, _ := s.store.ListFolders()
		var current bool
		for _, f := range folders {
			if f.ID == id {
				current = f.Enabled
				break
			}
		}
		if err := s.store.SetFolderEnabled(id, !current); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if s.onFolders != nil {
			s.onFolders()
		}
		writeJSON(w, 200, map[string]bool{"enabled": !current})
		return
	}
	http.Error(w, "not found", 404)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	records, err := s.store.ListConversions(limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, records)
}

func (s *Server) handleMatrix(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.registry.AvailableRules())
}

func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	total, saved, err := s.store.AnalyticsTotals()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"total_conversions": total,
		"bytes_saved":       saved,
	})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	// Reader goroutine to detect client close.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-closed:
			conn.Close()
			return
		case msg, ok := <-ch:
			if !ok {
				conn.Close()
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				conn.Close()
				return
			}
		}
	}
}

// Suppress unused import warnings if context becomes unused.
var _ = context.Background
var _ = fmt.Sprint
