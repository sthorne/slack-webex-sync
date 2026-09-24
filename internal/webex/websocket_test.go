package webex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sthorne/slack-webex-sync/internal/model"
)

func TestListenerRegistersAuthorizesAndAcks(t *testing.T) {
	client, _, _ := newTestClient(t)
	token, err := client.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	authMsgs := make(chan map[string]any, 1)
	acks := make(chan map[string]any, 1)
	upgrader := websocket.Upgrader{}
	var wsURL string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /devices", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://wdm/devices/1", "webSocketUrl": wsURL})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var auth map[string]any
		if conn.ReadJSON(&auth) != nil {
			return
		}
		authMsgs <- auth
		_ = conn.WriteMessage(websocket.TextMessage, frame("post", "p1", nil))
		var ack map[string]any
		if conn.ReadJSON(&ack) == nil {
			acks <- ack
		}
		<-r.Context().Done()
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	room := EncodeID("us", "ROOM", roomUUID)
	l := NewListener(client, server.URL+"/devices", []string{room}, "ME")
	var savedDevice string
	l.SaveDevice = func(u string) { savedDevice = u }
	connected := make(chan struct{}, 1)
	l.OnConnected = func() { connected <- struct{}{} }

	events := make(chan model.WebexEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx, func(ev model.WebexEvent) error { events <- ev; return nil })

	timeout := time.After(5 * time.Second)
	select {
	case auth := <-authMsgs:
		data, _ := auth["data"].(map[string]any)
		if auth["type"] != "authorization" || data["token"] != "Bearer "+token {
			t.Errorf("auth message = %v", auth)
		}
	case <-timeout:
		t.Fatal("no authorization message")
	}
	select {
	case <-connected:
	case <-timeout:
		t.Fatal("OnConnected not called")
	}
	select {
	case ev := <-events:
		if ev.Kind != model.WebexMessageCreated || ev.RoomID != room {
			t.Errorf("event = %+v", ev)
		}
	case <-timeout:
		t.Fatal("no event delivered")
	}
	select {
	case ack := <-acks:
		if ack["type"] != "ack" || ack["messageId"] != "mercury-1" {
			t.Errorf("ack = %v", ack)
		}
	case <-timeout:
		t.Fatal("no ack")
	}
	if savedDevice != "https://wdm/devices/1" {
		t.Errorf("device not saved: %q", savedDevice)
	}
}

func TestListenerDoesNotAckWhenQueueingFails(t *testing.T) {
	client, _, _ := newTestClient(t)
	acks := make(chan map[string]any, 1)
	upgrader := websocket.Upgrader{}
	var wsURL string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /devices", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://wdm/devices/1", "webSocketUrl": wsURL})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var auth map[string]any
		if conn.ReadJSON(&auth) != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, frame("post", "p1", nil))
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		var ack map[string]any
		if conn.ReadJSON(&ack) == nil {
			acks <- ack
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	wsURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	l := NewListener(client, server.URL+"/devices", []string{EncodeID("us", "ROOM", roomUUID)}, "ME")
	attempted := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx, func(model.WebexEvent) error {
		select {
		case attempted <- struct{}{}:
		default:
		}
		return errors.New("store down")
	})
	select {
	case <-attempted:
	case <-time.After(5 * time.Second):
		t.Fatal("event never offered to the sink")
	}
	select {
	case ack := <-acks:
		t.Fatalf("frame acknowledged although queueing failed: %v", ack)
	case <-time.After(time.Second):
	}
}
