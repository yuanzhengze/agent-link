// cowork dashboard — vanilla JS, no build step by design.
//
// Auth model (v1, no login flow): the API token and session name are
// pasted into the settings bar and kept in localStorage. Every fetch to a
// data endpoint sends `Authorization: Bearer <token>`; the WebSocket can't
// set headers so it passes the token as a `?token=` query param instead.
// The server derives `device` from the token; `session` is only needed
// client-side to build lock request bodies (owner = device:session).

(function () {
  "use strict";

  var LS_TOKEN = "cowork_token";
  var LS_SESSION = "cowork_session";

  var state = {
    token: localStorage.getItem(LS_TOKEN) || "",
    session: localStorage.getItem(LS_SESSION) || "",
    currentProjectId: null,
    currentProjectName: "",
    ws: null,
    wsReconnectTimer: null,
    wsReconnectDelay: 1000,
    contextMenuPath: null,
  };

  // ---- DOM refs ----------------------------------------------------------

  var el = {
    btnSettingsToggle: document.getElementById("btn-settings-toggle"),
    settingsBar: document.getElementById("settings-bar"),
    inputToken: document.getElementById("input-token"),
    inputSession: document.getElementById("input-session"),
    btnSaveSettings: document.getElementById("btn-save-settings"),
    settingsStatus: document.getElementById("settings-status"),
    globalError: document.getElementById("global-error"),

    btnBack: document.getElementById("btn-back"),
    currentProjectName: document.getElementById("current-project-name"),

    viewProjects: document.getElementById("view-projects"),
    viewProject: document.getElementById("view-project"),

    btnRefreshProjects: document.getElementById("btn-refresh-projects"),
    projectList: document.getElementById("project-list"),
    projectListEmpty: document.getElementById("project-list-empty"),

    btnRefreshTree: document.getElementById("btn-refresh-tree"),
    fileTree: document.getElementById("file-tree"),
    fileTreeEmpty: document.getElementById("file-tree-empty"),

    previewFrame: document.getElementById("preview-frame"),

    onlinePanel: document.getElementById("online-panel"),
    onlinePanelEmpty: document.getElementById("online-panel-empty"),

    btnClearAlerts: document.getElementById("btn-clear-alerts"),
    alertStream: document.getElementById("alert-stream"),

    contextMenu: document.getElementById("context-menu"),
  };

  // ---- settings -----------------------------------------------------------

  function loadSettingsIntoInputs() {
    el.inputToken.value = state.token;
    el.inputSession.value = state.session;
  }

  function saveSettings() {
    state.token = el.inputToken.value.trim();
    state.session = el.inputSession.value.trim();
    localStorage.setItem(LS_TOKEN, state.token);
    localStorage.setItem(LS_SESSION, state.session);
    el.settingsStatus.textContent = "Saved";
    setTimeout(function () {
      el.settingsStatus.textContent = "";
    }, 2000);
    clearGlobalError();
    loadProjects();
  }

  function toggleSettings() {
    el.settingsBar.classList.toggle("hidden");
  }

  function ensureConfigured() {
    if (!state.token) {
      showGlobalError("Set your API token in Settings above to load data.");
      el.settingsBar.classList.remove("hidden");
      return false;
    }
    return true;
  }

  // ---- error banner ---------------------------------------------------

  function showGlobalError(msg) {
    el.globalError.textContent = msg;
    el.globalError.classList.remove("hidden");
  }

  function clearGlobalError() {
    el.globalError.textContent = "";
    el.globalError.classList.add("hidden");
  }

  // ---- fetch helper -----------------------------------------------------

  // apiFetch wraps fetch() with the Bearer token, JSON parsing, and a
  // friendly inline message on 401 ("check your token"). It never throws
  // for HTTP error responses — callers get {ok, status, body} and decide
  // what to do (e.g. 409 lock conflicts carry useful body.error/body.owner).
  function apiFetch(path, opts) {
    opts = opts || {};
    var headers = Object.assign({}, opts.headers || {});
    if (state.token) {
      headers["Authorization"] = "Bearer " + state.token;
    }
    if (opts.body && !headers["Content-Type"]) {
      headers["Content-Type"] = "application/json";
    }
    return fetch(path, Object.assign({}, opts, { headers: headers }))
      .then(function (resp) {
        return resp
          .json()
          .catch(function () {
            return {};
          })
          .then(function (body) {
            if (resp.status === 401) {
              showGlobalError("Unauthorized — check your API token.");
            }
            return { ok: resp.ok, status: resp.status, body: body };
          });
      })
      .catch(function (err) {
        showGlobalError("Network error: " + err.message);
        return { ok: false, status: 0, body: {} };
      });
  }

  // ---- projects view ----------------------------------------------------

  function loadProjects() {
    if (!ensureConfigured()) return;
    apiFetch("/projects").then(function (res) {
      if (!res.ok) return;
      clearGlobalError();
      renderProjectList(res.body.projects || []);
    });
  }

  function renderProjectList(projects) {
    el.projectList.innerHTML = "";
    el.projectListEmpty.classList.toggle("hidden", projects.length > 0);
    projects.forEach(function (p) {
      var li = document.createElement("li");
      li.className = "project-item";
      li.innerHTML =
        '<span class="project-name"></span>' +
        '<span class="project-meta"></span>';
      li.querySelector(".project-name").textContent = p.name || p.id;
      li.querySelector(".project-meta").textContent =
        (p.created_at || "") + (p.head_commit ? " · " + p.head_commit.slice(0, 8) : "");
      li.addEventListener("click", function () {
        openProject(p.id, p.name || p.id);
      });
      el.projectList.appendChild(li);
    });
  }

  // ---- project view -----------------------------------------------------

  function openProject(id, name) {
    if (!ensureConfigured()) return;
    state.currentProjectId = id;
    state.currentProjectName = name;
    el.currentProjectName.textContent = name;
    el.btnBack.classList.remove("hidden");
    el.viewProjects.classList.add("hidden");
    el.viewProject.classList.remove("hidden");

    el.previewFrame.src = "/preview/" + encodeURIComponent(id) + "/";

    loadTree();
    loadOnline();
    connectWS(id);
  }

  function closeProject() {
    disconnectWS();
    state.currentProjectId = null;
    el.btnBack.classList.add("hidden");
    el.currentProjectName.textContent = "";
    el.viewProject.classList.add("hidden");
    el.viewProjects.classList.remove("hidden");
    el.previewFrame.src = "about:blank";
    loadProjects();
  }

  // ---- file tree + locks --------------------------------------------------

  function loadTree() {
    if (!state.currentProjectId) return;
    apiFetch("/projects/" + encodeURIComponent(state.currentProjectId) + "/tree").then(
      function (res) {
        if (!res.ok) return;
        clearGlobalError();
        renderFileTree(res.body.files || []);
      }
    );
  }

  function renderFileTree(files) {
    el.fileTree.innerHTML = "";
    el.fileTreeEmpty.classList.toggle("hidden", files.length > 0);
    files
      .slice()
      .sort(function (a, b) {
        return a.path.localeCompare(b.path);
      })
      .forEach(function (f) {
        var li = document.createElement("li");
        li.className = "file-item" + (f.locked ? " file-locked" : "");
        li.dataset.path = f.path;

        var pathSpan = document.createElement("span");
        pathSpan.className = "file-path";
        pathSpan.textContent = f.path;
        li.appendChild(pathSpan);

        if (f.locked) {
          var badge = document.createElement("span");
          badge.className = "lock-badge";
          badge.textContent = "🔒 " + f.owner;
          badge.title = "Locked by " + f.owner;
          li.appendChild(badge);
        }

        li.addEventListener("contextmenu", function (ev) {
          ev.preventDefault();
          openContextMenu(ev.pageX, ev.pageY, f.path);
        });

        el.fileTree.appendChild(li);
      });
  }

  function openContextMenu(x, y, path) {
    state.contextMenuPath = path;
    el.contextMenu.style.left = x + "px";
    el.contextMenu.style.top = y + "px";
    el.contextMenu.classList.remove("hidden");
  }

  function closeContextMenu() {
    el.contextMenu.classList.add("hidden");
    state.contextMenuPath = null;
  }

  function requireSession() {
    if (!state.session) {
      showGlobalError("Set a session name in Settings to acquire/release locks.");
      el.settingsBar.classList.remove("hidden");
      return false;
    }
    return true;
  }

  function doLockAction(action) {
    var path = state.contextMenuPath;
    closeContextMenu();
    if (!path || !state.currentProjectId) return;
    if (!requireSession()) return;

    var url = action === "acquire" ? "/locks/acquire" : "/locks/release";
    var payload = {
      project: state.currentProjectId,
      session: state.session,
      path: path,
    };
    if (action === "force-release") {
      payload.force = true;
    }

    apiFetch(url, { method: "POST", body: JSON.stringify(payload) }).then(function (res) {
      if (res.ok) {
        clearGlobalError();
      } else if (res.status === 409) {
        showGlobalError(
          (res.body.error || "conflict") + (res.body.owner ? " (owner: " + res.body.owner + ")" : "")
        );
      } else if (res.status !== 401) {
        showGlobalError(res.body.error || "request failed (" + res.status + ")");
      }
      loadTree();
    });
  }

  // ---- online panel -----------------------------------------------------

  function loadOnline() {
    if (!ensureConfigured()) return;
    apiFetch("/agents/list?all=true").then(function (res) {
      if (!res.ok) return;
      renderOnlinePanel(res.body.agents || []);
    });
  }

  function renderOnlinePanel(agents) {
    el.onlinePanel.innerHTML = "";
    el.onlinePanelEmpty.classList.toggle("hidden", agents.length > 0);
    agents.forEach(function (a) {
      var li = document.createElement("li");
      li.className = "agent-item";

      var head = document.createElement("div");
      head.className = "agent-head";
      var dot = document.createElement("span");
      dot.className = "status-dot " + (a.online ? "status-online" : "status-offline");
      head.appendChild(dot);
      var name = document.createElement("span");
      name.className = "agent-name";
      name.textContent = a.device;
      head.appendChild(name);
      li.appendChild(head);

      var sessions = a.session_status || [];
      sessions.forEach(function (s) {
        var row = document.createElement("div");
        row.className = "agent-session";
        row.textContent = s.name + ": " + s.current;
        li.appendChild(row);
      });

      el.onlinePanel.appendChild(li);
    });
  }

  // ---- alert stream -------------------------------------------------------

  function pushAlert(text, kind) {
    var li = document.createElement("li");
    li.className = "alert-item alert-" + (kind || "info");
    var time = document.createElement("span");
    time.className = "alert-time";
    time.textContent = new Date().toLocaleTimeString();
    li.appendChild(time);
    var body = document.createElement("span");
    body.className = "alert-text";
    body.textContent = text;
    li.appendChild(body);
    el.alertStream.insertBefore(li, el.alertStream.firstChild);

    // Cap the stream so a long session doesn't grow the DOM unbounded.
    while (el.alertStream.children.length > 200) {
      el.alertStream.removeChild(el.alertStream.lastChild);
    }
  }

  // ---- websocket ----------------------------------------------------------

  function connectWS(projectId) {
    disconnectWS();
    if (!state.token) return;

    var proto = location.protocol === "https:" ? "wss" : "ws";
    var url =
      proto + "://" + location.host + "/ws?project=" + encodeURIComponent(projectId) +
      "&token=" + encodeURIComponent(state.token);

    var ws = new WebSocket(url);
    state.ws = ws;

    ws.onopen = function () {
      state.wsReconnectDelay = 1000;
      pushAlert("Connected to " + projectId, "info");
    };

    ws.onmessage = function (ev) {
      var msg;
      try {
        msg = JSON.parse(ev.data);
      } catch (e) {
        return;
      }
      handleWSEvent(msg);
    };

    ws.onclose = function () {
      if (state.currentProjectId !== projectId) return; // left the project intentionally
      scheduleReconnect(projectId);
    };

    ws.onerror = function () {
      // onclose fires after onerror; reconnect is scheduled there.
    };
  }

  function scheduleReconnect(projectId) {
    clearTimeout(state.wsReconnectTimer);
    state.wsReconnectTimer = setTimeout(function () {
      if (state.currentProjectId === projectId) {
        connectWS(projectId);
      }
    }, state.wsReconnectDelay);
    state.wsReconnectDelay = Math.min(state.wsReconnectDelay * 2, 30000);
  }

  function disconnectWS() {
    clearTimeout(state.wsReconnectTimer);
    if (state.ws) {
      state.ws.onclose = null;
      state.ws.close();
      state.ws = null;
    }
  }

  function handleWSEvent(msg) {
    switch (msg.type) {
      case "file_changed":
        loadTree();
        break;
      case "lock_changed":
        loadTree();
        pushAlert(
          "Lock " + (msg.owner ? "held by " + msg.owner : "released") + " on " + msg.path,
          "lock"
        );
        break;
      case "conflict":
        pushAlert(
          "Conflict on " + (msg.path || "?") + (msg.by ? " by " + msg.by : ""),
          "conflict"
        );
        break;
      default:
        break;
    }
  }

  // ---- wiring --------------------------------------------------------------

  el.btnSettingsToggle.addEventListener("click", toggleSettings);
  el.btnSaveSettings.addEventListener("click", saveSettings);
  el.btnBack.addEventListener("click", closeProject);
  el.btnRefreshProjects.addEventListener("click", loadProjects);
  el.btnRefreshTree.addEventListener("click", loadTree);
  el.btnClearAlerts.addEventListener("click", function () {
    el.alertStream.innerHTML = "";
  });

  el.contextMenu.addEventListener("click", function (ev) {
    var action = ev.target && ev.target.dataset && ev.target.dataset.action;
    if (action) doLockAction(action);
  });
  document.addEventListener("click", function (ev) {
    if (!el.contextMenu.contains(ev.target)) closeContextMenu();
  });
  document.addEventListener("scroll", closeContextMenu, true);

  loadSettingsIntoInputs();
  if (!state.token) {
    el.settingsBar.classList.remove("hidden");
  }
  loadProjects();
})();
