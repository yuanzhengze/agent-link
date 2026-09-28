// Team-scoped cowork dashboard: projects, file tree with locks, live preview,
// online agents, and the change WebSocket. All requests go through the same
// team as CoworkTeams.current(); no credential is ever attached by hand — the
// session cookie and CSRF token are handled by CoworkAPI.
(function () {
  "use strict";

  var api = window.CoworkAPI;

  // Must match pkg/api seedIndexHTML. A brand-new project commits this file,
  // which is not a synced prototype.
  var SEED_INDEX_HTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>New Project</title>
</head>
<body>
  <h1>New Project</h1>
</body>
</html>
`;

  var state = {
    team: null,
    currentProjectId: null,
    currentProjectName: "",
    hasIndex: false,
    treeEpoch: 0,
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
    fileSyncCommand: document.getElementById("file-sync-command"),

    previewFrame: document.getElementById("preview-frame"),
    previewError: document.getElementById("preview-error"),
    previewEmpty: document.getElementById("preview-empty"),
    previewEmptyLead: document.getElementById("preview-empty-lead"),
    previewSyncCommand: document.getElementById("preview-sync-command"),
    btnCopySync: document.getElementById("btn-copy-sync"),

    projectModal: document.getElementById("project-modal"),
    formNewProject: document.getElementById("form-new-project"),
    inputProjectName: document.getElementById("input-project-name"),
    projectModalError: document.getElementById("project-modal-error"),
    btnProjectClose: document.getElementById("btn-project-close"),
    btnProjectCancel: document.getElementById("btn-project-cancel"),

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

  function showProjectModalError(msg) {
    if (!el.projectModalError) return;
    el.projectModalError.textContent = msg;
    el.projectModalError.classList.remove("hidden");
  }

  function clearProjectModalError() {
    if (!el.projectModalError) return;
    el.projectModalError.textContent = "";
    el.projectModalError.classList.add("hidden");
  }

  function openProjectModal() {
    if (!state.team || !el.projectModal) return;
    clearProjectModalError();
    if (el.formNewProject) el.formNewProject.reset();
    el.projectModal.classList.remove("hidden");
    if (el.inputProjectName) el.inputProjectName.focus();
  }

  function closeProjectModal() {
    if (!el.projectModal) return;
    el.projectModal.classList.add("hidden");
    clearProjectModalError();
  }

  function submitNewProject(ev) {
    ev.preventDefault();
    if (!state.team) return;
    var name = (el.inputProjectName && el.inputProjectName.value || "").trim();
    if (!name) {
      showProjectModalError("请输入项目名称。");
      return;
    }
    clearProjectModalError();
    api.request(teamURL("/projects"), {
      method: "POST",
      body: JSON.stringify({ name: name })
    }).then(function (res) {
      if (!res.ok) {
        showProjectModalError((res.body && res.body.error) || ("创建失败（" + res.status + "）"));
        return;
      }
      closeProjectModal();
      var project = res.body || {};
      if (project.id) openProject(project.id, project.name || name);
      else loadProjects();
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
    state.hasIndex = false;
    showPreviewWaiting();

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
    state.hasIndex = false;
    if (el.previewFrame) {
      el.previewFrame.src = "about:blank";
      el.previewFrame.classList.add("hidden");
    }
    if (el.previewEmpty) el.previewEmpty.classList.add("hidden");
    clearPreviewError();
  }

  function syncCommands() {
    var teamID = state.team ? state.team.id : "";
    var projectID = state.currentProjectId || "";
    return "agentlink team use " + teamID + "\nagentlink sync " + projectID + " ./prototype";
  }

  function showPreviewWaiting(lead) {
    if (el.previewEmptyLead) {
      el.previewEmptyLead.textContent = lead || "还没有可预览的内容";
    }
    if (el.fileSyncCommand) el.fileSyncCommand.textContent = syncCommands();
    if (el.previewSyncCommand) el.previewSyncCommand.textContent = syncCommands();
    if (el.previewEmpty) el.previewEmpty.classList.remove("hidden");
    if (el.previewFrame) {
      el.previewFrame.classList.add("hidden");
      el.previewFrame.src = "about:blank";
    }
  }

  function revealPreview() {
    if (el.previewEmpty) el.previewEmpty.classList.add("hidden");
    if (el.previewFrame) el.previewFrame.classList.remove("hidden");
    if (!el.previewFrame.src || el.previewFrame.src === "about:blank") loadPreview();
  }

  function isSeedIndex(content) {
    return content === SEED_INDEX_HTML;
  }

  function confirmNotSeed(epoch, projectId) {
    api.request(teamURL("/projects/" + encodeURIComponent(projectId) + "/snapshot")).then(function (res) {
      if (state.treeEpoch !== epoch || state.currentProjectId !== projectId) return;
      var files = (res.ok && res.body && res.body.files) || [];
      var index = null;
      files.forEach(function (f) {
        if (f.path === "index.html") index = f;
      });
      if (res.ok && files.length === 1 && index && isSeedIndex(index.content)) {
        state.hasIndex = false;
        showPreviewWaiting("目前只有起始页");
        return;
      }
      state.hasIndex = true;
      revealPreview();
    });
  }

  function updateEmptyState(files) {
    var epoch = ++state.treeEpoch;
    var hasIndex = files.some(function (f) {
      return f.path === "index.html" || f.path === "index.htm";
    });
    state.hasIndex = hasIndex;
    var commands = syncCommands();
    if (el.fileSyncCommand) el.fileSyncCommand.textContent = commands;
    if (el.previewSyncCommand) el.previewSyncCommand.textContent = commands;
    if (el.fileTreeEmpty) el.fileTreeEmpty.classList.toggle("hidden", files.length > 0);
    if (!hasIndex) {
      showPreviewWaiting("还没有可预览的内容");
      return;
    }
    if (files.length === 1 && files[0].path === "index.html") {
      state.hasIndex = false;
      showPreviewWaiting("目前只有起始页");
      confirmNotSeed(epoch, state.currentProjectId);
      return;
    }
    revealPreview();
  }

  function copySyncCommands() {
    var text = syncCommands();
    var done = function () {
      if (!el.btnCopySync) return;
      el.btnCopySync.textContent = "已复制";
      setTimeout(function () { el.btnCopySync.textContent = "复制命令"; }, 1500);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done).catch(function () {
        if (el.previewSyncCommand) {
          var range = document.createRange();
          range.selectNodeContents(el.previewSyncCommand);
          var selection = window.getSelection();
          selection.removeAllRanges();
          selection.addRange(range);
        }
      });
      return;
    }
    done();
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
      updateEmptyState((res.body && res.body.files) || []);
    });
  }

  function ownerLabel(owner) {
    if (!owner) return "";
    return owner.label || owner.username || owner.session_name || "locked";
  }

  function renderFileTree(files) {
    el.fileTree.innerHTML = "";
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
    if (!state.hasIndex) return;
    // Mint a fresh grant so a reload does not reuse an expired capability.
    loadPreview();
  }

  // ---- wiring ------------------------------------------------------------

  if (el.btnNewProject) el.btnNewProject.addEventListener("click", openProjectModal);
  if (el.formNewProject) el.formNewProject.addEventListener("submit", submitNewProject);
  if (el.btnProjectClose) el.btnProjectClose.addEventListener("click", closeProjectModal);
  if (el.btnProjectCancel) el.btnProjectCancel.addEventListener("click", closeProjectModal);
  if (el.projectModal) {
    el.projectModal.addEventListener("click", function (ev) {
      if (ev.target === el.projectModal) closeProjectModal();
    });
  }
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape" && el.projectModal && !el.projectModal.classList.contains("hidden")) {
      closeProjectModal();
    }
  });
  if (el.btnCopySync) el.btnCopySync.addEventListener("click", copySyncCommands);
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
