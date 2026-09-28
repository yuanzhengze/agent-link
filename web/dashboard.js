// Team-scoped cowork dashboard: projects, file tree with locks, live preview,
// online agents, and the change WebSocket. All requests go through the same
// team as CoworkTeams.current(); no credential is ever attached by hand — the
// session cookie and CSRF token are handled by CoworkAPI.
(function () {
  "use strict";

  var api = window.CoworkAPI;

  var state = {
    team: null,
    currentProjectId: null,
    currentProjectName: "",
    ws: null,
    wsReconnectTimer: null,
    wsReconnectDelay: 1000,
    contextMenuPath: null,
  };

  var el = {
    globalError: document.getElementById("global-error"),
    btnBack: document.getElementById("btn-back"),
    currentProjectName: document.getElementById("current-project-name"),

    viewProjects: document.getElementById("view-projects"),
    viewProject: document.getElementById("view-project"),

    btnNewProject: document.getElementById("btn-new-project"),
    btnRefreshProjects: document.getElementById("btn-refresh-projects"),
    projectList: document.getElementById("project-list"),
    projectListEmpty: document.getElementById("project-list-empty"),

    btnRefreshTree: document.getElementById("btn-refresh-tree"),
    fileTree: document.getElementById("file-tree"),
    fileTreeEmpty: document.getElementById("file-tree-empty"),

    previewFrame: document.getElementById("preview-frame"),
    previewError: document.getElementById("preview-error"),

    onlinePanel: document.getElementById("online-panel"),
    onlinePanelEmpty: document.getElementById("online-panel-empty"),

    btnClearAlerts: document.getElementById("btn-clear-alerts"),
    alertStream: document.getElementById("alert-stream"),

    contextMenu: document.getElementById("context-menu"),
  };

  function teamURL(suffix) {
    if (!state.team) throw new Error("no active team");
    return "/api/teams/" + encodeURIComponent(state.team.id) + suffix;
  }

  // ---- error banner ------------------------------------------------------

  function showGlobalError(msg) {
    el.globalError.textContent = msg;
    el.globalError.classList.remove("hidden");
  }

  function clearGlobalError() {
    el.globalError.textContent = "";
    el.globalError.classList.add("hidden");
  }

  // ---- lifecycle ---------------------------------------------------------

  function activate(team) {
    state.team = team;
    closeProject();
    clearGlobalError();
    loadProjects();
  }

  function teardown() {
    disconnectWS();
    state.team = null;
    state.currentProjectId = null;
  }

  // ---- projects ----------------------------------------------------------

  function loadProjects() {
    if (!state.team) return;
    api.request(teamURL("/projects")).then(function (res) {
      if (!res.ok) return;
      clearGlobalError();
      renderProjectList((res.body && res.body.projects) || []);
    });
  }

  function renderProjectList(projects) {
    el.projectList.innerHTML = "";
    el.projectListEmpty.classList.toggle("hidden", projects.length > 0);
    projects.forEach(function (p) {
      var li = document.createElement("li");
      li.className = "project-item";
      var nameSpan = document.createElement("span");
      nameSpan.className = "project-name";
      nameSpan.textContent = p.name || p.id;
      var metaSpan = document.createElement("span");
      metaSpan.className = "project-meta";
      metaSpan.textContent = (p.created_at || "") + (p.head_commit ? " · " + p.head_commit.slice(0, 8) : "");
      li.appendChild(nameSpan);
      li.appendChild(metaSpan);
      li.addEventListener("click", function () {
        openProject(p.id, p.name || p.id);
      });
      el.projectList.appendChild(li);
    });
  }

  function newProject() {
    if (!state.team) return;
    var name = window.prompt("New project name");
    if (!name) return;
    api.request(teamURL("/projects"), {
      method: "POST",
      body: JSON.stringify({ name: name.trim() })
    }).then(function (res) {
      if (res.ok) loadProjects();
      else showGlobalError((res.body && res.body.error) || ("Create failed (" + res.status + ")"));
    });
  }

  // ---- project view ------------------------------------------------------

  function openProject(id, name) {
    state.currentProjectId = id;
    state.currentProjectName = name;
    el.currentProjectName.textContent = name;
    el.btnBack.classList.remove("hidden");
    el.viewProjects.classList.add("hidden");
    el.viewProject.classList.remove("hidden");

    loadPreview();

    loadTree();
    loadAgents();
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
    clearPreviewError();
  }

  function showPreviewError(msg) {
    if (!el.previewError) return;
    el.previewError.textContent = msg;
    el.previewError.classList.remove("hidden");
  }

  function clearPreviewError() {
    if (!el.previewError) return;
    el.previewError.textContent = "";
    el.previewError.classList.add("hidden");
  }

  // Ask the server for the preview URL. When isolation is on, that URL is on
  // a different origin and carries a read-only grant, so prototype scripts
  // cannot use the viewer's session. The same-origin fallback is
  // /preview/<team>/<project>/ and is only returned when isolation is off.
  function loadPreview() {
    if (!state.team || !state.currentProjectId || !el.previewFrame) return;
    var projectId = state.currentProjectId;
    api.request(teamURL("/projects/" + encodeURIComponent(projectId) + "/preview-grant"), {
      method: "POST",
      body: "{}"
    }).then(function (res) {
      if (state.currentProjectId !== projectId) return;
      if (!res.ok || !res.body || !res.body.bootstrap_url) {
        showPreviewError((res.body && res.body.error) || ("Preview failed (" + res.status + ")"));
        el.previewFrame.src = "about:blank";
        return;
      }
      clearPreviewError();
      if (res.body.isolated) {
        el.previewFrame.setAttribute("sandbox", "allow-scripts allow-forms allow-popups allow-modals");
        el.previewFrame.setAttribute("referrerpolicy", "no-referrer");
      } else {
        el.previewFrame.removeAttribute("sandbox");
      }
      el.previewFrame.src = res.body.bootstrap_url;
    });
  }

  // ---- file tree + locks -------------------------------------------------

  function loadTree() {
    if (!state.currentProjectId) return;
    api.request(teamURL("/projects/" + encodeURIComponent(state.currentProjectId) + "/tree")).then(function (res) {
      if (!res.ok) return;
      clearGlobalError();
      renderFileTree((res.body && res.body.files) || []);
    });
  }

  function ownerLabel(owner) {
    if (!owner) return "";
    return owner.label || owner.username || owner.session_name || "locked";
  }

  function renderFileTree(files) {
    el.fileTree.innerHTML = "";
    el.fileTreeEmpty.classList.toggle("hidden", files.length > 0);
    files.slice().sort(function (a, b) {
      return a.path.localeCompare(b.path);
    }).forEach(function (f) {
      var li = document.createElement("li");
      li.className = "file-item" + (f.locked ? " file-locked" : "");
      li.dataset.path = f.path;

      var pathSpan = document.createElement("span");
      pathSpan.className = "file-path";
      pathSpan.textContent = f.path;
      li.appendChild(pathSpan);

      if (f.locked) {
        var label = ownerLabel(f.owner);
        var badge = document.createElement("span");
        badge.className = "lock-badge";
        badge.textContent = "🔒 " + label;
        badge.title = "Locked by " + label;
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

  function doLockAction(action) {
    var path = state.contextMenuPath;
    closeContextMenu();
    if (!path || !state.currentProjectId) return;

    var url = action === "acquire" ? teamURL("/locks/acquire") : teamURL("/locks/release");
    var payload = { project_id: state.currentProjectId, path: path };
    if (action === "force-release") payload.force = true;

    api.request(url, { method: "POST", body: JSON.stringify(payload) }).then(function (res) {
      if (res.ok) {
        clearGlobalError();
      } else if (res.status === 409) {
        var owner = res.body && res.body.owner;
        showGlobalError((res.body.error || "conflict") + (owner ? " (owner: " + ownerLabel(owner) + ")" : ""));
      } else if (res.status !== 401) {
        showGlobalError((res.body && res.body.error) || ("request failed (" + res.status + ")"));
      }
      loadTree();
    });
  }

  // ---- online agents -----------------------------------------------------

  function loadAgents() {
    if (!state.team) return;
    api.request(teamURL("/agents")).then(function (res) {
      if (!res.ok) return;
      renderOnlinePanel((res.body && res.body.agents) || []);
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
      name.textContent = (a.username || "") + " / " + (a.device_name || a.device_id || "");
      head.appendChild(name);
      li.appendChild(head);

      (a.sessions || []).forEach(function (sessionName) {
        var row = document.createElement("div");
        row.className = "agent-session";
        row.textContent = sessionName;
        li.appendChild(row);
      });

      el.onlinePanel.appendChild(li);
    });
  }

  // ---- alerts ------------------------------------------------------------

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
    while (el.alertStream.children.length > 200) {
      el.alertStream.removeChild(el.alertStream.lastChild);
    }
  }

  // ---- websocket ---------------------------------------------------------

  function connectWS(projectId) {
    disconnectWS();
    if (!state.team) return;

    var protocol = location.protocol === "https:" ? "wss:" : "ws:";
    var ws = new WebSocket(protocol + "//" + location.host +
      teamURL("/ws?project=" + encodeURIComponent(projectId)));
    state.ws = ws;

    ws.onopen = function () {
      state.wsReconnectDelay = 1000;
      pushAlert("Connected to " + state.currentProjectName, "info");
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
      if (state.currentProjectId !== projectId) return;
      scheduleReconnect(projectId);
    };
  }

  function scheduleReconnect(projectId) {
    clearTimeout(state.wsReconnectTimer);
    state.wsReconnectTimer = setTimeout(function () {
      if (state.currentProjectId === projectId) connectWS(projectId);
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
        reloadPreview();
        break;
      case "lock_changed":
        loadTree();
        pushAlert("Lock " + (msg.owner ? "held by " + ownerLabel(msg.owner) : "released") +
          " on " + (msg.path || "?"), "lock");
        break;
      case "conflict":
        pushAlert("Conflict on " + (msg.path || "?") +
          (msg.by ? " by " + ownerLabel(msg.by) : ""), "conflict");
        break;
      default:
        break;
    }
  }

  function reloadPreview() {
    // Mint a fresh grant so a reload does not reuse an expired capability.
    loadPreview();
  }

  // ---- wiring ------------------------------------------------------------

  if (el.btnNewProject) el.btnNewProject.addEventListener("click", newProject);
  if (el.btnRefreshProjects) el.btnRefreshProjects.addEventListener("click", loadProjects);
  if (el.btnBack) el.btnBack.addEventListener("click", function () { closeProject(); loadProjects(); });
  if (el.btnRefreshTree) el.btnRefreshTree.addEventListener("click", loadTree);
  if (el.btnClearAlerts) el.btnClearAlerts.addEventListener("click", function () {
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

  window.CoworkDashboard = {
    activate: activate,
    teardown: teardown,
    refresh: loadProjects,
  };
})();
