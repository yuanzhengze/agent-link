package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialWSV2 dials the shared authV2 test server's team WebSocket endpoint with
// the given headers (cookie or device Authorization). It returns the handshake
// response so callers can assert the HTTP status of a rejected upgrade.
func dialWSV2(t *testing.T, headers map[string]string, teamID, projectID string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	wsBase := "ws" + strings.TrimPrefix(authV2TS.URL, "http")
	opt := &websocket.DialOptions{HTTPHeader: http.Header{}}
	for k, v := range headers {
		opt.HTTPHeader.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := wsBase + "/api/teams/" + teamID + "/ws?project=" + projectID
	return websocket.Dial(ctx, url, opt)
}

// readEventV2 reads exactly one EventV2 off c within a short timeout.
func readEventV2(t *testing.T, c *websocket.Conn) EventV2 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("expected to read a message, got error: %v", err)
	}
	var ev EventV2
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("failed to decode EventV2 JSON: %v (raw=%s)", err, data)
	}
	return ev
}

// waitForSubscriberV2 polls until the hub reports want subscribers on key, so a
// broadcast in the test does not race the handler's async registration.
func waitForSubscriberV2(t *testing.T, key subscriptionKeyV2, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		authV2Srv.hubV2.mu.RLock()
		n := len(authV2Srv.hubV2.subs[key])
		authV2Srv.hubV2.mu.RUnlock()
		if n >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d subscriber(s) on %+v", want, key)
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func TestV2WSBrowserCookieReceivesOnlyOwnTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "wscookieown")
	team, _ := createTeamHTTP(t, session, "WS Cookie Own")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "WS Cookie Project")

	c, _, err := dialWSV2(t, map[string]string{"Cookie": withCookies(session)}, teamID, project.ID)
	if err != nil {
		t.Fatalf("cookie dial failed: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })

	key := subscriptionKeyV2{TeamID: teamID, ProjectID: project.ID}
	waitForSubscriberV2(t, key, 1)

	// An event for this (team, project) is delivered.
	want := EventV2{
		Type:       "file_changed",
		TeamID:     teamID,
		ProjectID:  project.ID,
		Path:       "index.html",
		HeadCommit: "deadbeef",
		At:         nowRFC3339(),
	}
	authV2Srv.hubV2.Broadcast(teamID, project.ID, want)
	got := readEventV2(t, c)
	if got.TeamID != teamID || got.ProjectID != project.ID || got.Path != "index.html" || got.HeadCommit != "deadbeef" {
		t.Fatalf("received event = %+v; want team %q project %q index.html", got, teamID, project.ID)
	}

	// An event for a different (team, project) must not reach this subscriber.
	// Run last: expectNoMessage's context timeout closes the connection.
	authV2Srv.hubV2.Broadcast("t_other_team", "p_other_project", EventV2{
		Type: "file_changed", TeamID: "t_other_team", ProjectID: "p_other_project", At: nowRFC3339(),
	})
	expectNoMessage(t, c, 400*time.Millisecond)
}

func TestV2WSDeviceAuthorizationHeader(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, userResult := registerTeamUser(t, "wsdevice")
	team, _ := createTeamHTTP(t, session, "WS Device Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "WS Device Project")
	username := userResult["user"].(map[string]any)["username"].(string)
	credential, _ := deviceLoginV2(t, username, "correct horse battery staple", "ws-device")

	c, _, err := dialWSV2(t, map[string]string{"Authorization": "Device " + credential}, teamID, project.ID)
	if err != nil {
		t.Fatalf("device dial failed: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })

	key := subscriptionKeyV2{TeamID: teamID, ProjectID: project.ID}
	waitForSubscriberV2(t, key, 1)

	want := EventV2{Type: "file_changed", TeamID: teamID, ProjectID: project.ID, Path: "page.html", At: nowRFC3339()}
	authV2Srv.hubV2.Broadcast(teamID, project.ID, want)
	got := readEventV2(t, c)
	if got.ProjectID != project.ID || got.Path != "page.html" {
		t.Fatalf("device subscriber received = %+v; want project %q page.html", got, project.ID)
	}
}

func TestV2WSRejectsNonMember(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "wsnonmemown")
	team, _ := createTeamHTTP(t, owner, "WS NonMember Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, owner, teamID, "WS NonMember Project")

	outsider, _ := registerTeamUser(t, "wsnonmemout")
	c, resp, err := dialWSV2(t, map[string]string{"Cookie": withCookies(outsider)}, teamID, project.ID)
	if err == nil {
		c.CloseNow()
		t.Fatal("expected non-member dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 handshake for non-member, got resp=%v err=%v", resp, err)
	}
}

func TestV2WSRejectsProjectFromOtherTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	owner, _ := registerTeamUser(t, "wsotherown")
	teamA, _ := createTeamHTTP(t, owner, "WS Other A")
	teamAID := teamA["id"].(string)
	projectA := createProjectV2HTTP(t, owner, teamAID, "WS Other Project")

	member, _ := registerTeamUser(t, "wsothermem")
	teamB, _ := createTeamHTTP(t, member, "WS Other B")
	teamBID := teamB["id"].(string)

	// Member of team B asks for team A's project through their own team route.
	c, resp, err := dialWSV2(t, map[string]string{"Cookie": withCookies(member)}, teamBID, projectA.ID)
	if err == nil {
		c.CloseNow()
		t.Fatal("expected cross-team project dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 handshake for cross-team project, got resp=%v err=%v", resp, err)
	}
}
