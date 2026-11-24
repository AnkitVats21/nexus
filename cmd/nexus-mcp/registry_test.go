package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"nexus/pkg/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func normJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The registry's declarations are what the server lists: name, description,
// input schema and the device-only marker.
func TestRegistryMatchesServerListing(t *testing.T) {
	reg := tools.NewRegistry(nil, nil, nil, nil)
	server, err := BuildMCPServer(ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != len(reg.Tools()) {
		t.Fatalf("server lists %d tools, registry has %d", len(listed.Tools), len(reg.Tools()))
	}
	for _, lt := range listed.Tools {
		rt, ok := reg.Get(lt.Name)
		if !ok {
			t.Errorf("server lists %q, not in registry", lt.Name)
			continue
		}
		if rt.Description != lt.Description {
			t.Errorf("%s: description differs", lt.Name)
		}
		if marked := lt.Meta["nexus/device_only"] == true; marked != rt.DeviceOnly {
			t.Errorf("%s: device_only server %v registry %v", lt.Name, marked, rt.DeviceOnly)
		}
		var rs any
		if err := json.Unmarshal(rt.InputSchema, &rs); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rs, normJSON(t, lt.InputSchema)) {
			t.Errorf("%s: input schema differs\nregistry: %s\nserver:   %v", lt.Name, rt.InputSchema, normJSON(t, lt.InputSchema))
		}
	}
}

// A local call returns what the server returns for the same arguments.
func TestRegistryCallMatchesServer(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"videoId":"abc","title":"Song","author":"Band","lengthSeconds":200}]`))
	}))
	defer api.Close()
	music := tools.NewMusicService(api.Client(), api.URL)
	reg := tools.NewRegistry(nil, nil, nil, music)
	server, err := BuildMCPServer(ServerConfig{MusicService: music})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	for _, tc := range []struct{ tool, args string }{
		{"music_search", `{"query":"song","limit":3}`},
		{"music_search", `{"query":"  "}`},
	} {
		tool, _ := reg.Get(tc.tool)
		local, err := tool.Call(ctx, json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		var args map[string]any
		json.Unmarshal([]byte(tc.args), &args)
		remote, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: args})
		if err != nil {
			// The server rejects arguments that break the schema; the
			// local call may not, but must not claim success.
			if !local.IsError {
				t.Errorf("%s %s: server rejected (%v), local did not fail", tc.tool, tc.args, err)
			}
			continue
		}
		if local.IsError != remote.IsError {
			t.Errorf("%s %s: IsError local %v server %v", tc.tool, tc.args, local.IsError, remote.IsError)
		}
		if remote.StructuredContent != nil || local.Structured != nil {
			if !reflect.DeepEqual(normJSON(t, local.Structured), normJSON(t, remote.StructuredContent)) {
				t.Errorf("%s: structured differs: %s vs %v", tc.tool, local.Structured, remote.StructuredContent)
			}
		}
		if len(remote.Content) > 0 {
			if tc, ok := remote.Content[0].(*mcp.TextContent); ok && tc.Text != local.Text {
				t.Errorf("text differs: %q vs %q", local.Text, tc.Text)
			}
		}
	}
}

func TestRegistryBadArguments(t *testing.T) {
	reg := tools.NewRegistry(nil, nil, nil, nil)
	tool, _ := reg.Get("music_search")
	if _, err := tool.Call(context.Background(), json.RawMessage(`{"query": 5}`)); err == nil {
		t.Fatal("want an error for a wrong argument type")
	}
}

// A light is declared here and run by the home hub: the server lists it and,
// called itself, says the hub runs it.
func TestServerDeclaresHubLight(t *testing.T) {
	server, err := BuildMCPServer(ServerConfig{Lights: []string{"work room light"}})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tl := range list.Tools {
		if tl.Name == "work_room_light" {
			found = true
		}
	}
	if !found {
		t.Fatal("work_room_light not listed")
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "work_room_light", Arguments: map[string]any{"power": "on"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || len(res.Content) == 0 || res.Content[0].(*mcp.TextContent).Text != "The work room light is controlled by the home hub, which is not connected right now." {
		t.Fatalf("call here: %+v", res)
	}
}
