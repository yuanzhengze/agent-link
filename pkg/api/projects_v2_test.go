package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	redisclient "github.com/team/agentlink/pkg/redis"
)

type projectV2TestView struct {
	ID         string `json:"id"`
	TeamID     string `json:"team_id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	HeadCommit string `json:"head_commit"`
}

var projectV2TestRoutesRegistered bool

func ensureProjectV2TestRoutes(t *testing.T) {
	t.Helper()
	wasNil := authV2TS == nil
	setupAuthV2TestServer(t)
	if wasNil {
		projectV2TestRoutesRegistered = false
	}
	if projectV2TestRoutesRegistered {
		return
	}

	authV2Srv.mux.Handle(
		"GET /api/teams/{team_id}/projects/{project_id}/test-load",
		authV2Srv.requireIdentity(authV2Srv.requireTeamRole()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := ActorFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			project, err := authV2Srv.loadTeamProject(r.Context(), actor.TeamID, r.PathValue("project_id"))
			switch {
			case errors.Is(err, errProjectNotFound):
				writeError(w, http.StatusNotFound, "not found")
			case err != nil:
				writeError(w, http.StatusInternalServerError, "internal error")
			default:
				writeJSON(w, http.StatusOK, project)
			}
		}))),
	)
	projectV2TestRoutesRegistered = true
}

func createProjectV2HTTP(t *testing.T, session *http.Response, teamID, name string) projectV2TestView {
	t.Helper()
	resp, body := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/projects", map[string]string{
		"name": name,
	}, session, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project expected 201, got %d body=%s", resp.StatusCode, body)
	}
	var project projectV2TestView
	if err := json.Unmarshal(body, &project); err != nil {
		t.Fatal(err)
	}
	if project.ID == "" {
		t.Fatalf("create project missing id: %s", body)
	}
	return project
}

func listProjectsV2HTTP(t *testing.T, session *http.Response, teamID string) []projectV2TestView {
	t.Helper()
	resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/projects", nil, session, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list projects expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var result struct {
		Projects []projectV2TestView `json:"projects"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Projects == nil {
		t.Fatalf("projects must encode as an array: %s", body)
	}
	return result.Projects
}

func projectV2TestID(n int) string {
	return fmt.Sprintf("%032x", n)
}

func fakeProjectGitV2(dir string) (string, error) {
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(seedIndexHTML), 0o644); err != nil {
		return "", err
	}
	return strings.Repeat("a", 40), nil
}

func stubProjectV2Creation(t *testing.T, ids []string, initializer func(string) (string, error)) *atomic.Int64 {
	t.Helper()
	if len(ids) == 0 {
		t.Fatal("stub project ids must not be empty")
	}
	oldGenerator := authV2Srv.projectIDGenerator
	oldInitializer := authV2Srv.projectGitInitializer
	var calls atomic.Int64
	authV2Srv.projectIDGenerator = func() string {
		call := calls.Add(1)
		index := int(call - 1)
		if index >= len(ids) {
			index = len(ids) - 1
		}
		return ids[index]
	}
	authV2Srv.projectGitInitializer = initializer
	t.Cleanup(func() {
		authV2Srv.projectIDGenerator = oldGenerator
		authV2Srv.projectGitInitializer = oldInitializer
	})
	return &calls
}

func rawProjectV2CookieRequest(
	t *testing.T,
	method string,
	path string,
	body []byte,
	session *http.Response,
	includeCSRF bool,
	extra map[string]string,
) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, authV2TS.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", withCookies(session))
	if includeCSRF {
		req.Header.Set("Origin", authTestOrigin)
		req.Header.Set("X-CSRF-Token", cookieByName(session.Cookies(), csrfCookieName).Value)
	}
	for key, value := range extra {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, respBody
}

func writeStoredProjectV2(t *testing.T, project projectV2TestView) {
	t.Helper()
	writeStoredProjectV2To(t, authV2Rdb.Client, project, project.ID)
}

func writeStoredProjectV2To(
	t *testing.T,
	rdb *goredis.Client,
	project projectV2TestView,
	creationToken string,
) {
	t.Helper()
	if err := rdb.HSet(
		context.Background(),
		projectV2Key(project.ID),
		"id", project.ID,
		"team_id", project.TeamID,
		"name", project.Name,
		"created_at", project.CreatedAt,
		"head_commit", project.HeadCommit,
		"creation_token", creationToken,
	).Err(); err != nil {
		t.Fatal(err)
	}
}

func runCreateProjectV2ScriptForTest(
	ctx context.Context,
	project projectV2TestView,
	creationToken string,
) (int64, error) {
	return createTeamProjectV2Script.Run(
		ctx,
		authV2Rdb,
		[]string{projectV2Key(project.ID), teamProjectsV2Key(project.TeamID)},
		project.ID,
		project.TeamID,
		project.Name,
		project.CreatedAt,
		project.HeadCommit,
		creationToken,
	).Int64()
}

func assertProjectV2NotPersisted(t *testing.T, teamID, projectID string) {
	t.Helper()
	ctx := context.Background()
	exists, err := authV2Rdb.Exists(ctx, projectV2Key(projectID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatalf("project metadata %s must not exist", projectID)
	}
	indexed, err := authV2Rdb.SIsMember(ctx, teamProjectsV2Key(teamID), projectID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if indexed {
		t.Fatalf("project %s must not be indexed", projectID)
	}
	if _, err := os.Stat(authV2Srv.projectDirV2(teamID, projectID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project directory %s must be removed, stat error = %v", projectID, err)
	}
}

func TestV2ProjectPersistReplayUsesCreationToken(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	project := projectV2TestView{
		ID:         projectV2TestID(91),
		TeamID:     "tm_project_replay",
		Name:       "Replay project",
		CreatedAt:  "2026-07-16T05:00:00Z",
		HeadCommit: strings.Repeat("9", 40),
	}
	creationToken := strings.Repeat("a", 32)
	dir := authV2Srv.projectDirV2(project.TeamID, project.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := runCreateProjectV2ScriptForTest(context.Background(), project, creationToken)
	if err != nil || first != 1 {
		t.Fatalf("first persist = %d, %v; want created result 1", first, err)
	}
	if err := authV2Rdb.SRem(
		context.Background(),
		teamProjectsV2Key(project.TeamID),
		project.ID,
	).Err(); err != nil {
		t.Fatal(err)
	}
	replay, err := runCreateProjectV2ScriptForTest(context.Background(), project, creationToken)
	if err != nil || replay != 2 {
		t.Fatalf("same-token replay = %d, %v; want idempotent result 2", replay, err)
	}

	originalFields, err := authV2Rdb.HGetAll(context.Background(), projectV2Key(project.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if originalFields["creation_token"] != creationToken {
		t.Fatalf("stored creation_token = %q; want %q", originalFields["creation_token"], creationToken)
	}
	if indexed, err := authV2Rdb.SIsMember(
		context.Background(),
		teamProjectsV2Key(project.TeamID),
		project.ID,
	).Result(); err != nil || !indexed {
		t.Fatalf("replayed project indexed=%v err=%v; want true", indexed, err)
	}

	differentToken, err := runCreateProjectV2ScriptForTest(
		context.Background(),
		project,
		strings.Repeat("b", 32),
	)
	if err != nil || differentToken != 0 {
		t.Fatalf("different-token persist = %d, %v; want collision 0", differentToken, err)
	}
	differentPayload := project
	differentPayload.Name = "Changed payload"
	sameTokenDifferentPayload, err := runCreateProjectV2ScriptForTest(
		context.Background(),
		differentPayload,
		creationToken,
	)
	if err != nil || sameTokenDifferentPayload != 0 {
		t.Fatalf("different-payload persist = %d, %v; want collision 0", sameTokenDifferentPayload, err)
	}

	afterFields, err := authV2Rdb.HGetAll(context.Background(), projectV2Key(project.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterFields, originalFields) {
		t.Fatalf("collision/replay changed metadata: got %v want %v", afterFields, originalFields)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "original" {
		t.Fatalf("collision/replay changed worktree: body=%q err=%v", body, err)
	}
}

type projectV2CreateScriptFault int

const (
	projectV2CreateScriptPass projectV2CreateScriptFault = iota
	projectV2CreateScriptFailBefore
	projectV2CreateScriptFailAfter
)

type projectV2CreateScriptStep struct {
	fault projectV2CreateScriptFault
	after func()
}

type projectV2UncertainResultHook struct {
	mu                 sync.Mutex
	createSteps        []projectV2CreateScriptStep
	createCalls        int
	capturedCreateArgs [][]any
	failVerification   bool
	injected           atomic.Bool
}

func (h *projectV2UncertainResultHook) DialHook(next goredis.DialHook) goredis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (h *projectV2UncertainResultHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		args := cmd.Args()
		isCreateScript := cmd.Name() == "evalsha" &&
			len(args) > 1 &&
			fmt.Sprint(args[1]) == createTeamProjectV2Script.Hash()
		if isCreateScript {
			h.mu.Lock()
			call := h.createCalls
			h.createCalls++
			h.capturedCreateArgs = append(h.capturedCreateArgs, append([]any(nil), args...))
			step := projectV2CreateScriptStep{}
			if call < len(h.createSteps) {
				step = h.createSteps[call]
			}
			h.mu.Unlock()

			if step.fault == projectV2CreateScriptFailBefore {
				h.injected.Store(true)
				if step.after != nil {
					step.after()
				}
				return io.ErrUnexpectedEOF
			}
			err := next(ctx, cmd)
			if err != nil {
				return err
			}
			if step.after != nil {
				step.after()
			}
			if step.fault == projectV2CreateScriptFailAfter {
				h.injected.Store(true)
				return io.ErrUnexpectedEOF
			}
			return nil
		}

		if h.failVerification &&
			(cmd.Name() == "eval" || cmd.Name() == "evalsha") &&
			h.createCallCount() >= 2 {
			h.injected.Store(true)
			return io.ErrUnexpectedEOF
		}
		return next(ctx, cmd)
	}
}

func (h *projectV2UncertainResultHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		return next(ctx, cmds)
	}
}

func (h *projectV2UncertainResultHook) createCallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.createCalls
}

func (h *projectV2UncertainResultHook) capturedProject(
	t *testing.T,
	call int,
) (projectV2TestView, string) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if call < 0 || call >= len(h.capturedCreateArgs) {
		t.Fatalf("captured create call %d missing", call)
	}
	args := h.capturedCreateArgs[call]
	if len(args) < 11 {
		t.Fatalf("captured create args = %v; want script keys and six fields", args)
	}
	return projectV2TestView{
		ID:         fmt.Sprint(args[5]),
		TeamID:     fmt.Sprint(args[6]),
		Name:       fmt.Sprint(args[7]),
		CreatedAt:  fmt.Sprint(args[8]),
		HeadCommit: fmt.Sprint(args[9]),
	}, fmt.Sprint(args[10])
}

func newProjectV2HookedStore(
	t *testing.T,
	hook goredis.Hook,
) (*Server, *goredis.Client, *goredis.Client) {
	t.Helper()
	raw := goredis.NewClient(&goredis.Options{Addr: "localhost:6379", DB: 15})
	if err := raw.Ping(context.Background()).Err(); err != nil {
		raw.Close()
		t.Skip("redis not available")
	}
	if err := raw.FlushDB(context.Background()).Err(); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := createTeamProjectV2Script.Load(context.Background(), raw).Err(); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.AddHook(hook)
	inspector := goredis.NewClient(&goredis.Options{Addr: "localhost:6379", DB: 15})
	t.Cleanup(func() {
		_ = inspector.FlushDB(context.Background()).Err()
		_ = inspector.Close()
		_ = raw.Close()
	})
	return &Server{
		rdb:                   &redisclient.Client{Client: raw},
		dataDir:               t.TempDir(),
		projectGitInitializer: fakeProjectGitV2,
	}, raw, inspector
}

func TestV2ProjectCreateAndListAreTeamScoped(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectscope")
	team, _ := createTeamHTTP(t, session, "Scoped Projects")
	teamID := team["id"].(string)

	created := createProjectV2HTTP(t, session, teamID, "  Team site  ")
	if created.TeamID != teamID {
		t.Fatalf("created project team_id = %q; want %q", created.TeamID, teamID)
	}
	if created.Name != "Team site" {
		t.Fatalf("created project name = %q; want trimmed name", created.Name)
	}
	if len(created.ID) < 32 {
		t.Fatalf("created project id = %q; want at least 128 bits", created.ID)
	}
	if created.CreatedAt == "" || created.HeadCommit == "" {
		t.Fatalf("created project missing metadata: %+v", created)
	}

	projects := listProjectsV2HTTP(t, session, teamID)
	if len(projects) != 1 || projects[0] != created {
		t.Fatalf("listed projects = %+v; want only %+v", projects, created)
	}

	stored, err := authV2Rdb.HGetAll(context.Background(), "agentlink:v2:project:"+created.ID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if stored["id"] != created.ID || stored["team_id"] != teamID || stored["name"] != created.Name ||
		stored["created_at"] != created.CreatedAt || stored["head_commit"] != created.HeadCommit {
		t.Fatalf("stored project = %v; want response fields", stored)
	}
	if !validProjectCreationTokenV2(stored["creation_token"]) {
		t.Fatalf("stored creation_token = %q; want 128-bit lowercase hex", stored["creation_token"])
	}
	indexed, err := authV2Rdb.SIsMember(context.Background(), "agentlink:v2:team:"+teamID+":projects", created.ID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("created project missing from team index")
	}
}

func TestV2ProjectListDoesNotLeakOtherTeam(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectisolation")
	teamA, _ := createTeamHTTP(t, session, "Project Team A")
	teamB, _ := createTeamHTTP(t, session, "Project Team B")
	teamAID := teamA["id"].(string)
	teamBID := teamB["id"].(string)

	projectA := createProjectV2HTTP(t, session, teamAID, "A only")
	projectB := createProjectV2HTTP(t, session, teamBID, "B only")

	listA := listProjectsV2HTTP(t, session, teamAID)
	if len(listA) != 1 || listA[0].ID != projectA.ID || listA[0].TeamID != teamAID {
		t.Fatalf("team A projects = %+v; want only project A", listA)
	}
	listB := listProjectsV2HTTP(t, session, teamBID)
	if len(listB) != 1 || listB[0].ID != projectB.ID || listB[0].TeamID != teamBID {
		t.Fatalf("team B projects = %+v; want only project B", listB)
	}
}

func TestV2ProjectIDFromOtherTeamReturns404(t *testing.T) {
	ensureProjectV2TestRoutes(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectload")
	teamA, _ := createTeamHTTP(t, session, "Load Team A")
	teamB, _ := createTeamHTTP(t, session, "Load Team B")
	teamAID := teamA["id"].(string)
	teamBID := teamB["id"].(string)
	projectB := createProjectV2HTTP(t, session, teamBID, "Secret B")

	resp, body := teamJSON(
		t,
		http.MethodGet,
		"/api/teams/"+teamAID+"/projects/"+projectB.ID+"/test-load",
		nil,
		session,
		nil,
	)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-team project load expected 404, got %d body=%s", resp.StatusCode, body)
	}
	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result["error"] != "not found" {
		t.Fatalf("cross-team project error = %q; want not found", result["error"])
	}
}

func TestV2ProjectWorkTreesUseTeamDirectory(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectpath")
	team, _ := createTeamHTTP(t, session, "Path Team")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Nested work tree")

	wantDir := filepath.Join(authV2Srv.dataDir, "work", teamID, project.ID)
	if got := authV2Srv.projectDirV2(teamID, project.ID); got != wantDir {
		t.Fatalf("projectDirV2() = %q; want %q", got, wantDir)
	}
	for _, relative := range []string{".git", "index.html"} {
		if _, err := os.Stat(filepath.Join(wantDir, relative)); err != nil {
			t.Fatalf("expected %s in team project directory: %v", relative, err)
		}
	}
	if _, err := os.Stat(filepath.Join(authV2Srv.dataDir, "work", project.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy flat project directory must not exist, stat error = %v", err)
	}
}

func TestV2ProjectRejectsUnsafeTeamPathWithoutFilesystemWrites(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	dataDir := t.TempDir()
	projectID := projectV2TestID(95)
	escapeRoot := filepath.Join(filepath.Dir(dataDir), "project-escape-"+projectID)
	t.Cleanup(func() { _ = os.RemoveAll(escapeRoot) })
	teamID := filepath.Join("..", "..", filepath.Base(escapeRoot))
	srv := &Server{
		rdb:                   authV2Rdb,
		dataDir:               dataDir,
		projectIDGenerator:    func() string { return projectID },
		projectGitInitializer: fakeProjectGitV2,
	}

	if _, err := srv.createTeamProjectV2(context.Background(), teamID, "Unsafe team"); err == nil {
		t.Fatal("unsafe team path expected an error")
	}
	if _, err := os.Stat(filepath.Join(escapeRoot, projectID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe team path wrote outside work root: %v", err)
	}
}

func TestV2ProjectRejectsSymlinkTeamDirectory(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	dataDir := t.TempDir()
	outside := t.TempDir()
	teamID := "tm_symlink_project"
	projectID := projectV2TestID(96)
	workDir := filepath.Join(dataDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, teamID)); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		rdb:                   authV2Rdb,
		dataDir:               dataDir,
		projectIDGenerator:    func() string { return projectID },
		projectGitInitializer: fakeProjectGitV2,
	}

	if _, err := srv.createTeamProjectV2(context.Background(), teamID, "Symlink team"); err == nil {
		t.Fatal("symlink team directory expected an error")
	}
	if _, err := os.Stat(filepath.Join(outside, projectID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink team directory wrote outside work root: %v", err)
	}
}

func TestV2ProjectCookieCreateRequiresCSRF(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectcsrf")
	team, _ := createTeamHTTP(t, session, "Project CSRF")
	teamID := team["id"].(string)
	projectID := projectV2TestID(101)
	stubProjectV2Creation(t, []string{projectID}, fakeProjectGitV2)

	resp, body := rawProjectV2CookieRequest(
		t,
		http.MethodPost,
		"/api/teams/"+teamID+"/projects",
		[]byte(`{"name":"No CSRF"}`),
		session,
		false,
		nil,
	)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie create without CSRF expected 403, got %d body=%s", resp.StatusCode, body)
	}
	assertProjectV2NotPersisted(t, teamID, projectID)
}

func TestV2ProjectDeviceCanCreateAndListWithoutCSRF(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, userResult := registerTeamUser(t, "projectdevice")
	team, _ := createTeamHTTP(t, session, "Device Projects")
	teamID := team["id"].(string)
	username := userResult["user"].(map[string]any)["username"].(string)

	loginResp, loginBody := authJSON(t, http.MethodPost, "/api/auth/device-login", map[string]string{
		"username":    username,
		"password":    "correct horse battery staple",
		"device_name": "project-device",
	}, nil)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("device login expected 200, got %d body=%s", loginResp.StatusCode, loginBody)
	}
	var loginResult map[string]any
	if err := json.Unmarshal(loginBody, &loginResult); err != nil {
		t.Fatal(err)
	}
	credential := loginResult["device_credential"].(string)
	headers := map[string]string{"Authorization": "Device " + credential}

	createResp, createBody := authJSON(t, http.MethodPost, "/api/teams/"+teamID+"/projects", map[string]string{
		"name": "Device project",
	}, headers)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("device create expected 201, got %d body=%s", createResp.StatusCode, createBody)
	}
	listResp, listBody := authJSON(t, http.MethodGet, "/api/teams/"+teamID+"/projects", nil, headers)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("device list expected 200, got %d body=%s", listResp.StatusCode, listBody)
	}
	var list struct {
		Projects []projectV2TestView `json:"projects"`
	}
	if err := json.Unmarshal(listBody, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Projects) != 1 || list.Projects[0].Name != "Device project" {
		t.Fatalf("device project list = %+v; want created project", list.Projects)
	}
}

func TestV2ProjectBearerIsRejectedWithoutCookieFallback(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectbearer")
	team, _ := createTeamHTTP(t, session, "Bearer Projects")
	teamID := team["id"].(string)
	resp, body := rawProjectV2CookieRequest(
		t,
		http.MethodPost,
		"/api/teams/"+teamID+"/projects",
		[]byte(`{"name":"Bearer project"}`),
		session,
		true,
		map[string]string{"Authorization": "Bearer sk_live_" + strings.Repeat("b", 64)},
	)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("legacy Bearer expected 401, got %d body=%s", resp.StatusCode, body)
	}
}

func TestV2ProjectMustChangePasswordBlocksRoutes(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, userResult := registerTeamUser(t, "projectmust")
	team, _ := createTeamHTTP(t, session, "Must Change Projects")
	teamID := team["id"].(string)
	username := userResult["user"].(map[string]any)["username"].(string)
	tempPassword, err := authV2Srv.authService.ResetPassword(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	loginResp, loginBody := loginAuthUser(t, username, tempPassword, nil)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("temporary password login expected 200, got %d body=%s", loginResp.StatusCode, loginBody)
	}

	resp, body := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/projects", map[string]string{
		"name": "Blocked project",
	}, loginResp, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("must-change create expected 403, got %d body=%s", resp.StatusCode, body)
	}
}

func TestV2ProjectMemberCanCreateAndList(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	ownerSession, _ := registerTeamUser(t, "projectowner")
	team, invite := createTeamHTTP(t, ownerSession, "Member Projects")
	teamID := team["id"].(string)
	memberSession, _ := registerTeamUser(t, "projectmember")
	joinTeamHTTP(t, memberSession, teamID, invite)

	project := createProjectV2HTTP(t, memberSession, teamID, "Member created")
	projects := listProjectsV2HTTP(t, memberSession, teamID)
	if len(projects) != 1 || projects[0].ID != project.ID {
		t.Fatalf("member project list = %+v; want project %q", projects, project.ID)
	}
}

func TestV2ProjectUsesActorTeamNotClientTeamHints(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectactor")
	teamA, _ := createTeamHTTP(t, session, "Actor Project A")
	teamB, _ := createTeamHTTP(t, session, "Actor Project B")
	teamAID := teamA["id"].(string)
	teamBID := teamB["id"].(string)

	resp, body := teamJSON(
		t,
		http.MethodPost,
		"/api/teams/"+teamAID+"/projects?team_id="+teamBID,
		map[string]string{"name": "URL team wins"},
		session,
		map[string]string{"X-Team-ID": teamBID},
	)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("spoofed team hints create expected 201, got %d body=%s", resp.StatusCode, body)
	}
	var project projectV2TestView
	if err := json.Unmarshal(body, &project); err != nil {
		t.Fatal(err)
	}
	if project.TeamID != teamAID {
		t.Fatalf("project team_id = %q; want Actor team %q", project.TeamID, teamAID)
	}
	indexedInB, err := authV2Rdb.SIsMember(context.Background(), teamProjectsV2Key(teamBID), project.ID).Result()
	if err != nil {
		t.Fatal(err)
	}
	if indexedInB {
		t.Fatal("client-provided team hint indexed project in the wrong team")
	}
}

func TestV2ProjectBodyValidation(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectbody")
	team, _ := createTeamHTTP(t, session, "Body Validation")
	teamID := team["id"].(string)
	path := "/api/teams/" + teamID + "/projects"

	testCases := []struct {
		name string
		body []byte
	}{
		{name: "oversized", body: []byte(`{"name":"` + strings.Repeat("x", 65*1024) + `"}`)},
		{name: "unknown field", body: []byte(`{"name":"valid","team_id":"spoofed"}`)},
		{name: "trailing JSON", body: []byte(`{"name":"valid"} {"name":"second"}`)},
		{name: "empty", body: []byte(`{"name":"   "}`)},
		{name: "too long unicode", body: []byte(`{"name":"` + strings.Repeat("界", 65) + `"}`)},
		{name: "control", body: []byte(`{"name":"bad\u0000name"}`)},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			resp, body := rawProjectV2CookieRequest(t, http.MethodPost, path, testCase.body, session, true, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid body expected 400, got %d body=%s", resp.StatusCode, body)
			}
		})
	}
	size, err := authV2Rdb.SCard(context.Background(), teamProjectsV2Key(teamID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Fatalf("invalid requests persisted %d projects", size)
	}
}

func TestV2ProjectRedisTypeErrorsAreAtomicAndCleanDirectories(t *testing.T) {
	for testIndex, testCase := range []struct {
		name      string
		corrupt   func(t *testing.T, teamID, projectID string)
		assertion func(t *testing.T, teamID, projectID string)
	}{
		{
			name: "project hash wrong type",
			corrupt: func(t *testing.T, _ string, projectID string) {
				t.Helper()
				if err := authV2Rdb.Set(context.Background(), projectV2Key(projectID), "occupied", 0).Err(); err != nil {
					t.Fatal(err)
				}
			},
			assertion: func(t *testing.T, teamID, projectID string) {
				t.Helper()
				value, err := authV2Rdb.Get(context.Background(), projectV2Key(projectID)).Result()
				if err != nil || value != "occupied" {
					t.Fatalf("wrong-type project key changed: value=%q err=%v", value, err)
				}
				exists, err := authV2Rdb.Exists(context.Background(), teamProjectsV2Key(teamID)).Result()
				if err != nil || exists != 0 {
					t.Fatalf("team index was partially written: exists=%d err=%v", exists, err)
				}
			},
		},
		{
			name: "team set wrong type",
			corrupt: func(t *testing.T, teamID, _ string) {
				t.Helper()
				if err := authV2Rdb.Set(context.Background(), teamProjectsV2Key(teamID), "occupied", 0).Err(); err != nil {
					t.Fatal(err)
				}
			},
			assertion: func(t *testing.T, teamID, projectID string) {
				t.Helper()
				exists, err := authV2Rdb.Exists(context.Background(), projectV2Key(projectID)).Result()
				if err != nil || exists != 0 {
					t.Fatalf("project hash was partially written: exists=%d err=%v", exists, err)
				}
				value, err := authV2Rdb.Get(context.Background(), teamProjectsV2Key(teamID)).Result()
				if err != nil || value != "occupied" {
					t.Fatalf("wrong-type team index changed: value=%q err=%v", value, err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			setupAuthV2TestServer(t)
			cleanupAuthV2Keys(t)
			session, _ := registerTeamUser(t, fmt.Sprintf("ptype%d", testIndex))
			team, _ := createTeamHTTP(t, session, "Type Error Project")
			teamID := team["id"].(string)
			projectID := projectV2TestID(201)
			stubProjectV2Creation(t, []string{projectID}, fakeProjectGitV2)
			testCase.corrupt(t, teamID, projectID)

			resp, body := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/projects", map[string]string{
				"name": "Must fail atomically",
			}, session, nil)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("wrong Redis type expected 500, got %d body=%s", resp.StatusCode, body)
			}
			testCase.assertion(t, teamID, projectID)
			if _, err := os.Stat(authV2Srv.projectDirV2(teamID, projectID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed project directory must be removed, stat error = %v", err)
			}
		})
	}
}

func TestV2ProjectGitInitializationFailureCleansDirectory(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectgitfail")
	team, _ := createTeamHTTP(t, session, "Git Failure")
	teamID := team["id"].(string)
	projectID := projectV2TestID(301)
	stubProjectV2Creation(t, []string{projectID}, func(string) (string, error) {
		return "", errors.New("injected git failure")
	})

	resp, body := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/projects", map[string]string{
		"name": "Git must fail",
	}, session, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("git failure expected 500, got %d body=%s", resp.StatusCode, body)
	}
	assertProjectV2NotPersisted(t, teamID, projectID)
}

func TestV2ProjectInvalidGitHeadCleansDirectoryWithoutPersisting(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectbadhead")
	team, _ := createTeamHTTP(t, session, "Invalid Git Head")
	teamID := team["id"].(string)
	projectID := projectV2TestID(302)
	stubProjectV2Creation(t, []string{projectID}, func(string) (string, error) {
		return "not-a-git-object-id", nil
	})

	resp, body := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/projects", map[string]string{
		"name": "Invalid head",
	}, session, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("invalid git head expected 500, got %d body=%s", resp.StatusCode, body)
	}
	assertProjectV2NotPersisted(t, teamID, projectID)
}

func TestV2ProjectIDCollisionPreservesExistingDataAndRetries(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectcollision")
	team, _ := createTeamHTTP(t, session, "Collision Projects")
	teamID := team["id"].(string)
	collisionID := projectV2TestID(401)
	successID := projectV2TestID(402)
	calls := stubProjectV2Creation(t, []string{collisionID, successID}, fakeProjectGitV2)

	original := projectV2TestView{
		ID:         collisionID,
		TeamID:     teamID,
		Name:       "Existing project",
		CreatedAt:  "2026-07-16T00:00:00Z",
		HeadCommit: strings.Repeat("b", 40),
	}
	writeStoredProjectV2(t, original)
	originalFields, err := authV2Rdb.HGetAll(context.Background(), projectV2Key(collisionID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	collisionDir := authV2Srv.projectDirV2(teamID, collisionID)
	if err := os.MkdirAll(collisionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(collisionDir, "existing.txt")
	if err := os.WriteFile(marker, []byte("do not overwrite"), 0o644); err != nil {
		t.Fatal(err)
	}

	created := createProjectV2HTTP(t, session, teamID, "Retry succeeds")
	if created.ID != successID {
		t.Fatalf("created id = %q; want retry id %q", created.ID, successID)
	}
	if calls.Load() != 2 {
		t.Fatalf("generator calls = %d; want 2", calls.Load())
	}
	afterFields, err := authV2Rdb.HGetAll(context.Background(), projectV2Key(collisionID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterFields, originalFields) {
		t.Fatalf("collision metadata changed: got %v want %v", afterFields, originalFields)
	}
	markerBody, err := os.ReadFile(marker)
	if err != nil || string(markerBody) != "do not overwrite" {
		t.Fatalf("collision directory changed: body=%q err=%v", markerBody, err)
	}
}

func TestV2ProjectMetadataOnlyCollisionCleansAttemptAndRetries(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectmetacollision")
	team, _ := createTeamHTTP(t, session, "Metadata Collision")
	teamID := team["id"].(string)
	collisionID := projectV2TestID(411)
	successID := projectV2TestID(412)
	stubProjectV2Creation(t, []string{collisionID, successID}, fakeProjectGitV2)
	original := projectV2TestView{
		ID:         collisionID,
		TeamID:     teamID,
		Name:       "Metadata only",
		CreatedAt:  "2026-07-16T00:00:00Z",
		HeadCommit: strings.Repeat("c", 40),
	}
	writeStoredProjectV2(t, original)

	created := createProjectV2HTTP(t, session, teamID, "Metadata retry")
	if created.ID != successID {
		t.Fatalf("created id = %q; want %q", created.ID, successID)
	}
	if _, err := os.Stat(authV2Srv.projectDirV2(teamID, collisionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata collision attempt directory remains: %v", err)
	}
	stored, err := authV2Rdb.HGetAll(context.Background(), projectV2Key(collisionID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if stored["name"] != original.Name || stored["head_commit"] != original.HeadCommit {
		t.Fatalf("existing metadata changed: %v", stored)
	}
}

func TestV2ProjectFiveIDCollisionsLeaveNoNewState(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectexhaust")
	team, _ := createTeamHTTP(t, session, "Exhausted Projects")
	teamID := team["id"].(string)
	ids := make([]string, projectV2CreateAttempts)
	originals := make(map[string]map[string]string, projectV2CreateAttempts)
	for index := range ids {
		ids[index] = projectV2TestID(500 + index)
		writeStoredProjectV2(t, projectV2TestView{
			ID:         ids[index],
			TeamID:     teamID,
			Name:       fmt.Sprintf("Occupied %d", index),
			CreatedAt:  "2026-07-16T00:00:00Z",
			HeadCommit: strings.Repeat("d", 40),
		})
		fields, err := authV2Rdb.HGetAll(context.Background(), projectV2Key(ids[index])).Result()
		if err != nil {
			t.Fatal(err)
		}
		originals[ids[index]] = fields
	}
	calls := stubProjectV2Creation(t, ids, fakeProjectGitV2)

	resp, body := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/projects", map[string]string{
		"name": "Never created",
	}, session, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("five collisions expected 500, got %d body=%s", resp.StatusCode, body)
	}
	if calls.Load() != projectV2CreateAttempts {
		t.Fatalf("generator calls = %d; want %d", calls.Load(), projectV2CreateAttempts)
	}
	for _, projectID := range ids {
		fields, err := authV2Rdb.HGetAll(context.Background(), projectV2Key(projectID)).Result()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fields, originals[projectID]) {
			t.Fatalf("occupied metadata %s changed: got %v want %v", projectID, fields, originals[projectID])
		}
		if _, err := os.Stat(authV2Srv.projectDirV2(teamID, projectID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("collision attempt directory %s remains: %v", projectID, err)
		}
	}
	indexExists, err := authV2Rdb.Exists(context.Background(), teamProjectsV2Key(teamID)).Result()
	if err != nil || indexExists != 0 {
		t.Fatalf("collision exhaustion created team index: exists=%d err=%v", indexExists, err)
	}
}

func TestV2ProjectCanceledInitialPersistenceUsesFreshSettlement(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	teamID := "tm_project_cancel"
	projectID := projectV2TestID(551)
	srv := &Server{
		rdb:                   authV2Rdb,
		dataDir:               t.TempDir(),
		projectIDGenerator:    func() string { return projectID },
		projectGitInitializer: fakeProjectGitV2,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	project, err := srv.createTeamProjectV2(ctx, teamID, "Canceled project")
	if err != nil {
		t.Fatalf("fresh settlement should recover canceled initial persistence: %v", err)
	}
	if project.ID != projectID {
		t.Fatalf("settled project id = %q; want %q", project.ID, projectID)
	}
	if _, err := os.Stat(srv.projectDirV2(teamID, projectID)); err != nil {
		t.Fatalf("settled project tree missing: %v", err)
	}
	fields, err := authV2Rdb.HGetAll(context.Background(), projectV2Key(projectID)).Result()
	if err != nil || fields["id"] != projectID || !validProjectCreationTokenV2(fields["creation_token"]) {
		t.Fatalf("settled project metadata = %v err=%v", fields, err)
	}
	indexed, err := authV2Rdb.SIsMember(
		context.Background(),
		teamProjectsV2Key(teamID),
		projectID,
	).Result()
	if err != nil || !indexed {
		t.Fatalf("settled project indexed=%v err=%v; want true", indexed, err)
	}
}

func TestV2ProjectLostCreateReplySettlesIdempotently(t *testing.T) {
	hook := &projectV2UncertainResultHook{
		createSteps: []projectV2CreateScriptStep{
			{fault: projectV2CreateScriptFailAfter},
		},
	}
	srv, _, inspector := newProjectV2HookedStore(t, hook)
	teamID := "tm_project_uncertain_match"
	projectID := projectV2TestID(552)
	srv.projectIDGenerator = func() string { return projectID }

	project, err := srv.createTeamProjectV2(context.Background(), teamID, "Uncertain persisted")
	if err != nil {
		t.Fatalf("matching persisted hash should recover as success: %v", err)
	}
	if !hook.injected.Load() {
		t.Fatal("test hook did not inject an uncertain Redis result")
	}
	if project.ID != projectID {
		t.Fatalf("recovered project id = %q; want %q", project.ID, projectID)
	}
	if calls := hook.createCallCount(); calls != 2 {
		t.Fatalf("create script calls = %d; want initial write plus idempotent settlement", calls)
	}
	initialProject, initialToken := hook.capturedProject(t, 0)
	settledProject, settledToken := hook.capturedProject(t, 1)
	if initialProject != settledProject || initialToken != settledToken {
		t.Fatalf(
			"settlement changed request: initial=%+v token=%q settled=%+v token=%q",
			initialProject,
			initialToken,
			settledProject,
			settledToken,
		)
	}
	if _, err := os.Stat(srv.projectDirV2(teamID, projectID)); err != nil {
		t.Fatalf("matching persisted project tree was removed: %v", err)
	}
	fields, err := inspector.HGetAll(context.Background(), projectV2Key(projectID)).Result()
	if err != nil || fields["id"] != projectID || fields["team_id"] != teamID {
		t.Fatalf("persisted metadata = %v err=%v", fields, err)
	}
	indexed, err := inspector.SIsMember(
		context.Background(),
		teamProjectsV2Key(teamID),
		projectID,
	).Result()
	if err != nil || !indexed {
		t.Fatalf("persisted index membership=%v err=%v; want true", indexed, err)
	}
}

func TestV2ProjectLostSettlementReplyUsesAtomicVerification(t *testing.T) {
	hook := &projectV2UncertainResultHook{
		createSteps: []projectV2CreateScriptStep{
			{fault: projectV2CreateScriptFailAfter},
			{fault: projectV2CreateScriptFailAfter},
		},
	}
	srv, _, inspector := newProjectV2HookedStore(t, hook)
	teamID := "tm_project_uncertain_verify_match"
	projectID := projectV2TestID(553)
	srv.projectIDGenerator = func() string { return projectID }

	project, err := srv.createTeamProjectV2(context.Background(), teamID, "Verified persisted")
	if err != nil {
		t.Fatalf("atomic verification should recover matching project: %v", err)
	}
	if project.ID != projectID || hook.createCallCount() != 2 {
		t.Fatalf("verified project=%+v create calls=%d", project, hook.createCallCount())
	}
	if _, err := os.Stat(srv.projectDirV2(teamID, projectID)); err != nil {
		t.Fatalf("verified project tree missing: %v", err)
	}
	indexed, err := inspector.SIsMember(
		context.Background(),
		teamProjectsV2Key(teamID),
		projectID,
	).Result()
	if err != nil || !indexed {
		t.Fatalf("verified project indexed=%v err=%v; want true", indexed, err)
	}
}

func TestV2ProjectUnverifiableRedisResultLeavesTree(t *testing.T) {
	hook := &projectV2UncertainResultHook{
		createSteps: []projectV2CreateScriptStep{
			{fault: projectV2CreateScriptFailAfter},
			{fault: projectV2CreateScriptFailAfter},
		},
		failVerification: true,
	}
	srv, _, inspector := newProjectV2HookedStore(t, hook)
	teamID := "tm_project_uncertain_unknown"
	projectID := projectV2TestID(554)
	srv.projectIDGenerator = func() string { return projectID }

	if _, err := srv.createTeamProjectV2(
		context.Background(),
		teamID,
		"Unverifiable persisted",
	); !errors.Is(err, errProjectStoreInconsistent) {
		t.Fatalf("unverifiable Redis result error = %v; want store error", err)
	}
	if !hook.injected.Load() {
		t.Fatal("test hook did not inject an uncertain Redis result")
	}
	if calls := hook.createCallCount(); calls != 2 {
		t.Fatalf("create script calls = %d; want initial and settlement", calls)
	}
	if _, err := os.Stat(srv.projectDirV2(teamID, projectID)); err != nil {
		t.Fatalf("unverifiable project tree must be preserved: %v", err)
	}

	fields, err := inspector.HGetAll(context.Background(), projectV2Key(projectID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if fields["team_id"] != teamID {
		t.Fatalf("persisted metadata = %v; want team %q", fields, teamID)
	}
}

func TestV2ProjectUncertainAbsentVerificationPreservesTreeForLateCreate(t *testing.T) {
	hook := &projectV2UncertainResultHook{
		createSteps: []projectV2CreateScriptStep{
			{fault: projectV2CreateScriptFailBefore},
			{fault: projectV2CreateScriptFailBefore},
		},
	}
	srv, raw, inspector := newProjectV2HookedStore(t, hook)
	teamID := "tm_project_uncertain_absent"
	projectID := projectV2TestID(555)
	creationToken := strings.Repeat("1", 32)
	srv.projectIDGenerator = func() string { return projectID }
	srv.projectTokenGenerator = func() (string, error) { return creationToken, nil }

	if _, err := srv.createTeamProjectV2(
		context.Background(),
		teamID,
		"Late project",
	); !errors.Is(err, errProjectStoreInconsistent) {
		t.Fatalf("absent verification error = %v; want store error", err)
	}
	if calls := hook.createCallCount(); calls != 2 {
		t.Fatalf("create script calls = %d; want uncertain initial and settlement", calls)
	}
	dir := srv.projectDirV2(teamID, projectID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("absent verification must preserve work tree: %v", err)
	}
	exists, err := inspector.Exists(context.Background(), projectV2Key(projectID)).Result()
	if err != nil || exists != 0 {
		t.Fatalf("project metadata exists=%d err=%v before late command; want absent", exists, err)
	}

	captured, capturedToken := hook.capturedProject(t, 0)
	if capturedToken != creationToken {
		t.Fatalf("captured token = %q; want %q", capturedToken, creationToken)
	}
	result, err := createTeamProjectV2Script.Run(
		context.Background(),
		raw,
		[]string{projectV2Key(captured.ID), teamProjectsV2Key(captured.TeamID)},
		captured.ID,
		captured.TeamID,
		captured.Name,
		captured.CreatedAt,
		captured.HeadCommit,
		capturedToken,
	).Int64()
	if err != nil || result != 1 {
		t.Fatalf("late same-token create result=%d err=%v; want 1", result, err)
	}
	indexed, err := inspector.SIsMember(
		context.Background(),
		teamProjectsV2Key(teamID),
		projectID,
	).Result()
	if err != nil || !indexed {
		t.Fatalf("late project indexed=%v err=%v; want true", indexed, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("late project metadata points to missing tree: %v", err)
	}
}

func TestV2ProjectVerifiedForeignTokenCleansOnlyCrossTeamAttemptAndRetries(t *testing.T) {
	hook := &projectV2UncertainResultHook{
		createSteps: []projectV2CreateScriptStep{
			{fault: projectV2CreateScriptFailBefore},
			{fault: projectV2CreateScriptFailBefore},
		},
	}
	srv, _, inspector := newProjectV2HookedStore(t, hook)
	foreignTeamID := "tm_project_foreign_owner"
	attemptTeamID := "tm_project_foreign_attempt"
	collisionID := projectV2TestID(556)
	successID := projectV2TestID(557)
	foreignToken := strings.Repeat("2", 32)
	foreign := projectV2TestView{
		ID:         collisionID,
		TeamID:     foreignTeamID,
		Name:       "Foreign project",
		CreatedAt:  "2026-07-16T06:00:00Z",
		HeadCommit: strings.Repeat("b", 40),
	}
	writeStoredProjectV2To(t, inspector, foreign, foreignToken)
	if err := inspector.SAdd(
		context.Background(),
		teamProjectsV2Key(foreignTeamID),
		collisionID,
	).Err(); err != nil {
		t.Fatal(err)
	}
	foreignDir := srv.projectDirV2(foreignTeamID, collisionID)
	if err := os.MkdirAll(foreignDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignMarker := filepath.Join(foreignDir, "owner")
	if err := os.WriteFile(foreignMarker, []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	originalFields, err := inspector.HGetAll(context.Background(), projectV2Key(collisionID)).Result()
	if err != nil {
		t.Fatal(err)
	}

	var idCalls atomic.Int64
	srv.projectIDGenerator = func() string {
		if idCalls.Add(1) == 1 {
			return collisionID
		}
		return successID
	}
	var tokenCalls atomic.Int64
	srv.projectTokenGenerator = func() (string, error) {
		if tokenCalls.Add(1) == 1 {
			return strings.Repeat("3", 32), nil
		}
		return strings.Repeat("4", 32), nil
	}

	project, err := srv.createTeamProjectV2(context.Background(), attemptTeamID, "Attempt project")
	if err != nil {
		t.Fatalf("foreign-token collision should retry: %v", err)
	}
	if project.ID != successID || idCalls.Load() != 2 || tokenCalls.Load() != 2 {
		t.Fatalf(
			"created=%+v id calls=%d token calls=%d; want second attempt",
			project,
			idCalls.Load(),
			tokenCalls.Load(),
		)
	}
	if _, err := os.Stat(srv.projectDirV2(attemptTeamID, collisionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-team collision attempt tree remains: %v", err)
	}
	if body, err := os.ReadFile(foreignMarker); err != nil || string(body) != "foreign" {
		t.Fatalf("foreign work tree changed: body=%q err=%v", body, err)
	}
	afterFields, err := inspector.HGetAll(context.Background(), projectV2Key(collisionID)).Result()
	if err != nil || !reflect.DeepEqual(afterFields, originalFields) {
		t.Fatalf("foreign metadata changed: got=%v want=%v err=%v", afterFields, originalFields, err)
	}
	foreignIndexed, err := inspector.SIsMember(
		context.Background(),
		teamProjectsV2Key(foreignTeamID),
		collisionID,
	).Result()
	if err != nil || !foreignIndexed {
		t.Fatalf("foreign index membership=%v err=%v; want true", foreignIndexed, err)
	}
	attemptIndexed, err := inspector.SIsMember(
		context.Background(),
		teamProjectsV2Key(attemptTeamID),
		collisionID,
	).Result()
	if err != nil || attemptIndexed {
		t.Fatalf("attempt collision indexed=%v err=%v; want false", attemptIndexed, err)
	}
	if _, err := os.Stat(srv.projectDirV2(attemptTeamID, successID)); err != nil {
		t.Fatalf("successful retry tree missing: %v", err)
	}
}

func TestV2ProjectMatchingHashWithBrokenIndexPreservesTree(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(t *testing.T, inspector *goredis.Client, indexKey string)
		assert func(t *testing.T, inspector *goredis.Client, indexKey string)
	}{
		{
			name: "missing index",
			mutate: func(t *testing.T, inspector *goredis.Client, indexKey string) {
				t.Helper()
				if err := inspector.Del(context.Background(), indexKey).Err(); err != nil {
					t.Fatal(err)
				}
			},
			assert: func(t *testing.T, inspector *goredis.Client, indexKey string) {
				t.Helper()
				exists, err := inspector.Exists(context.Background(), indexKey).Result()
				if err != nil || exists != 0 {
					t.Fatalf("missing index exists=%d err=%v; want absent", exists, err)
				}
			},
		},
		{
			name: "wrong-type index",
			mutate: func(t *testing.T, inspector *goredis.Client, indexKey string) {
				t.Helper()
				if err := inspector.Del(context.Background(), indexKey).Err(); err != nil {
					t.Fatal(err)
				}
				if err := inspector.Set(context.Background(), indexKey, "corrupt", 0).Err(); err != nil {
					t.Fatal(err)
				}
			},
			assert: func(t *testing.T, inspector *goredis.Client, indexKey string) {
				t.Helper()
				value, err := inspector.Get(context.Background(), indexKey).Result()
				if err != nil || value != "corrupt" {
					t.Fatalf("wrong-type index value=%q err=%v", value, err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var inspector *goredis.Client
			teamID := "tm_project_broken_index_" + strings.ReplaceAll(testCase.name, " ", "_")
			projectID := projectV2TestID(558)
			creationToken := strings.Repeat("5", 32)
			indexKey := teamProjectsV2Key(teamID)
			hook := &projectV2UncertainResultHook{
				createSteps: []projectV2CreateScriptStep{
					{
						fault: projectV2CreateScriptFailAfter,
						after: func() {
							testCase.mutate(t, inspector, indexKey)
						},
					},
					{fault: projectV2CreateScriptFailBefore},
				},
			}
			srv, _, gotInspector := newProjectV2HookedStore(t, hook)
			inspector = gotInspector
			srv.projectIDGenerator = func() string { return projectID }
			srv.projectTokenGenerator = func() (string, error) { return creationToken, nil }

			if _, err := srv.createTeamProjectV2(
				context.Background(),
				teamID,
				"Broken index project",
			); !errors.Is(err, errProjectStoreInconsistent) {
				t.Fatalf("broken index error = %v; want store error", err)
			}
			if calls := hook.createCallCount(); calls != 2 {
				t.Fatalf("create script calls=%d; want initial and uncertain settlement", calls)
			}
			if _, err := os.Stat(srv.projectDirV2(teamID, projectID)); err != nil {
				t.Fatalf("broken index must preserve work tree: %v", err)
			}
			fields, err := inspector.HGetAll(context.Background(), projectV2Key(projectID)).Result()
			if err != nil ||
				fields["id"] != projectID ||
				fields["team_id"] != teamID ||
				fields["creation_token"] != creationToken {
				t.Fatalf("matching metadata=%v err=%v", fields, err)
			}
			testCase.assert(t, inspector, indexKey)
		})
	}
}

func TestV2ProjectConcurrentSameGlobalIDHasOneOwner(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectconcurrent")
	teamA, _ := createTeamHTTP(t, session, "Concurrent A")
	teamB, _ := createTeamHTTP(t, session, "Concurrent B")
	teamIDs := []string{teamA["id"].(string), teamB["id"].(string)}
	projectID := projectV2TestID(601)
	stubProjectV2Creation(t, []string{projectID}, fakeProjectGitV2)

	type result struct {
		teamID string
		status int
		body   []byte
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, len(teamIDs))
	var workers sync.WaitGroup
	for _, teamID := range teamIDs {
		teamID := teamID
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			req, err := http.NewRequest(
				http.MethodPost,
				authV2TS.URL+"/api/teams/"+teamID+"/projects",
				strings.NewReader(`{"name":"Concurrent project"}`),
			)
			if err != nil {
				results <- result{teamID: teamID, err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Cookie", withCookies(session))
			req.Header.Set("Origin", authTestOrigin)
			req.Header.Set("X-CSRF-Token", cookieByName(session.Cookies(), csrfCookieName).Value)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- result{teamID: teamID, err: err}
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			results <- result{teamID: teamID, status: resp.StatusCode, body: body, err: readErr}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	var winner string
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		switch result.status {
		case http.StatusCreated:
			if winner != "" {
				t.Fatalf("more than one concurrent create succeeded: %q and %q", winner, result.teamID)
			}
			winner = result.teamID
		case http.StatusInternalServerError:
		default:
			t.Fatalf("concurrent create for %s returned %d body=%s", result.teamID, result.status, result.body)
		}
	}
	if winner == "" {
		t.Fatal("no concurrent create succeeded")
	}
	stored, err := authV2Rdb.HGetAll(context.Background(), projectV2Key(projectID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if stored["team_id"] != winner {
		t.Fatalf("stored team_id = %q; want winning team %q", stored["team_id"], winner)
	}
	for _, teamID := range teamIDs {
		indexed, err := authV2Rdb.SIsMember(context.Background(), teamProjectsV2Key(teamID), projectID).Result()
		if err != nil {
			t.Fatal(err)
		}
		if indexed != (teamID == winner) {
			t.Fatalf("team %s indexed=%v; winner=%s", teamID, indexed, winner)
		}
		_, statErr := os.Stat(authV2Srv.projectDirV2(teamID, projectID))
		if teamID == winner && statErr != nil {
			t.Fatalf("winning work tree missing: %v", statErr)
		}
		if teamID != winner && !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("losing work tree remains: %v", statErr)
		}
	}
}

func TestV2ProjectListRemovesStaleAndMismatchedEntriesAndSorts(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectstale")
	teamA, _ := createTeamHTTP(t, session, "Stale A")
	teamB, _ := createTeamHTTP(t, session, "Stale B")
	teamAID := teamA["id"].(string)
	teamBID := teamB["id"].(string)
	earlyLow := projectV2TestID(701)
	earlyHigh := projectV2TestID(702)
	later := projectV2TestID(703)
	otherTeam := projectV2TestID(704)
	stale := projectV2TestID(705)
	head := strings.Repeat("e", 40)
	for _, project := range []projectV2TestView{
		{ID: later, TeamID: teamAID, Name: "Later", CreatedAt: "2026-07-16T02:00:00Z", HeadCommit: head},
		{ID: earlyHigh, TeamID: teamAID, Name: "Early high", CreatedAt: "2026-07-16T01:00:00Z", HeadCommit: head},
		{ID: earlyLow, TeamID: teamAID, Name: "Early low", CreatedAt: "2026-07-16T01:00:00Z", HeadCommit: head},
		{ID: otherTeam, TeamID: teamBID, Name: "Other team", CreatedAt: "2026-07-16T00:00:00Z", HeadCommit: head},
	} {
		writeStoredProjectV2(t, project)
	}
	if err := authV2Rdb.SAdd(
		context.Background(),
		teamProjectsV2Key(teamAID),
		later,
		earlyHigh,
		earlyLow,
		otherTeam,
		stale,
	).Err(); err != nil {
		t.Fatal(err)
	}

	projects := listProjectsV2HTTP(t, session, teamAID)
	got := make([]string, 0, len(projects))
	for _, project := range projects {
		got = append(got, project.ID)
		if project.TeamID != teamAID {
			t.Fatalf("list leaked project from team %q: %+v", project.TeamID, project)
		}
	}
	want := []string{earlyLow, earlyHigh, later}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted project ids = %v; want %v", got, want)
	}
	for _, removed := range []string{otherTeam, stale} {
		indexed, err := authV2Rdb.SIsMember(context.Background(), teamProjectsV2Key(teamAID), removed).Result()
		if err != nil {
			t.Fatal(err)
		}
		if indexed {
			t.Fatalf("stale/mismatched id %s was not removed", removed)
		}
	}
}

func TestV2ProjectEmptyListIsJSONArray(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectempty")
	team, _ := createTeamHTTP(t, session, "Empty Projects")
	teamID := team["id"].(string)
	resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/projects", nil, session, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty list expected 200, got %d body=%s", resp.StatusCode, body)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if string(result["projects"]) != "[]" {
		t.Fatalf("empty projects JSON = %s; want []", result["projects"])
	}
}

func TestV2ProjectCorruptStoreReturns500(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectcorrupt")
	team, _ := createTeamHTTP(t, session, "Corrupt Projects")
	teamID := team["id"].(string)
	projectID := projectV2TestID(801)
	writeStoredProjectV2(t, projectV2TestView{
		ID:         projectID,
		TeamID:     teamID,
		Name:       "Corrupt project",
		CreatedAt:  "not-a-time",
		HeadCommit: strings.Repeat("f", 40),
	})
	if err := authV2Rdb.SAdd(context.Background(), teamProjectsV2Key(teamID), projectID).Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := authV2Srv.loadTeamProject(context.Background(), teamID, projectID); !errors.Is(err, errProjectStoreInconsistent) {
		t.Fatalf("load corrupt project error = %v; want errProjectStoreInconsistent", err)
	}
	resp, body := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/projects", nil, session, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("corrupt project list expected 500, got %d body=%s", resp.StatusCode, body)
	}
}

func TestV2ProjectResponsesExposeOnlyPublicFields(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectpublic")
	team, _ := createTeamHTTP(t, session, "Public Response")
	teamID := team["id"].(string)
	createResp, createBody := teamJSON(t, http.MethodPost, "/api/teams/"+teamID+"/projects", map[string]string{
		"name": "Public fields",
	}, session, nil)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create expected 201, got %d body=%s", createResp.StatusCode, createBody)
	}
	var project map[string]any
	if err := json.Unmarshal(createBody, &project); err != nil {
		t.Fatal(err)
	}
	wantFields := map[string]bool{
		"id": true, "team_id": true, "name": true, "created_at": true, "head_commit": true,
	}
	if len(project) != len(wantFields) {
		t.Fatalf("create response fields = %v; want only %v", project, wantFields)
	}
	for field := range project {
		if !wantFields[field] {
			t.Fatalf("create response exposed unexpected field %q", field)
		}
	}
	payload := string(createBody)
	for _, forbidden := range []string{
		authV2Srv.dataDir,
		"api_key",
		"device_credential",
		"token",
		"internal_path",
		"agentlink:project:",
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("create response exposed %q: %s", forbidden, createBody)
		}
	}

	listResp, listBody := teamJSON(t, http.MethodGet, "/api/teams/"+teamID+"/projects", nil, session, nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list expected 200, got %d body=%s", listResp.StatusCode, listBody)
	}
	for _, forbidden := range []string{
		authV2Srv.dataDir,
		"api_key",
		"device_credential",
		"token",
		"internal_path",
	} {
		if strings.Contains(string(listBody), forbidden) {
			t.Fatalf("list response exposed %q: %s", forbidden, listBody)
		}
	}
}

func TestV2ProjectGitUsesRepositoryLocalCoworkIdentity(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	session, _ := registerTeamUser(t, "projectgitidentity")
	team, _ := createTeamHTTP(t, session, "Git Identity")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Identity project")
	dir := authV2Srv.projectDirV2(teamID, project.ID)

	for key, want := range map[string]string{
		"user.name":  "cowork",
		"user.email": "cowork@localhost",
	} {
		out, err := exec.Command("git", "-C", dir, "config", "--local", "--get", key).Output()
		if err != nil {
			t.Fatalf("read local git %s: %v", key, err)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Fatalf("local git %s = %q; want %q", key, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project creation wrote global git config: %v", err)
	}
}

func TestV2ProjectUsesOnlyV2ProjectKeys(t *testing.T) {
	setupAuthV2TestServer(t)
	cleanupAuthV2Keys(t)

	session, _ := registerTeamUser(t, "projectkeys")
	team, _ := createTeamHTTP(t, session, "Project Keys")
	teamID := team["id"].(string)
	project := createProjectV2HTTP(t, session, teamID, "Key project")
	ctx := context.Background()

	for _, legacyKey := range []string{"agentlink:project:" + project.ID, "agentlink:projects"} {
		exists, err := authV2Rdb.Exists(ctx, legacyKey).Result()
		if err != nil {
			t.Fatal(err)
		}
		if exists != 0 {
			t.Fatalf("legacy project key %q was written", legacyKey)
		}
	}
	for _, wantKey := range []string{projectV2Key(project.ID), teamProjectsV2Key(teamID)} {
		exists, err := authV2Rdb.Exists(ctx, wantKey).Result()
		if err != nil || exists != 1 {
			t.Fatalf("v2 project key %q missing: exists=%d err=%v", wantKey, exists, err)
		}
	}
}

func TestV2ProjectProductionMuxOmitsLoadTestRoute(t *testing.T) {
	srv := NewWithOptions(ServerOptions{
		DataDir:   t.TempDir(),
		PublicURL: "http://localhost:8080",
	})
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/teams/team_abc/projects/"+projectV2TestID(901)+"/test-load",
		nil,
	)
	resp := httptest.NewRecorder()
	srv.authMiddleware(srv.mux).ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound && resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("production project test route returned %d; want 404 or 405", resp.Code)
	}
}
