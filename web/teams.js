// Team onboarding, switching, and (Task 3) member administration for the cowork
// GUI. Only the non-sensitive current team id is persisted; invite codes are
// shown once and never stored.
(function (root) {
  "use strict";

  function invitePairText(teamID, code) {
    return "Team ID: " + teamID + "\nInvite code: " + code;
  }

  // parseInvitePair splits one pasted block into the two join fields.
  // English labels (Team ID / Invite code) and Chinese labels (团队 ID / 邀请码)
  // are both accepted, with ":" or "：". Missing either label returns null so a
  // normal single-value paste is left alone.
  function parseInvitePair(text) {
    if (text == null) return null;
    var src = String(text).replace(/^\uFEFF/, "").replace(/[\u00a0\u3000]/g, " ");
    var teamID = captureLabeled(src, "(?:team\\s*id|团队\\s*id)");
    var inviteCode = captureLabeled(src, "(?:invite\\s*code|邀请码)");
    if (!teamID || !inviteCode) return null;
    return { teamID: cleanToken(teamID), inviteCode: cleanToken(inviteCode) };
  }

  function captureLabeled(src, label) {
    var match = src.match(new RegExp(label + "\\s*[:：]\\s*(\\S+)", "i"));
    return match ? match[1] : null;
  }

  function cleanToken(token) {
    return String(token).replace(/[,，.。;；]+$/g, "");
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      parseInvitePair: parseInvitePair,
      invitePairText: invitePairText
    };
  }
  if (!root || !root.document) return;

  var api = root.CoworkAPI;
  var LS_TEAM = "cowork_current_team_id";

  var teams = [];
  var currentTeam = null;

  var el = {
    switcher: document.getElementById("team-switcher"),
    formCreate: document.getElementById("form-create-team"),
    formJoin: document.getElementById("form-join-team"),
    inputName: document.getElementById("input-team-name"),
    inputTeamID: document.getElementById("input-team-id"),
    inputInvite: document.getElementById("input-invite-code"),
    onboardingError: document.getElementById("onboarding-error"),

    inviteModal: document.getElementById("invite-modal"),
    invitePair: document.getElementById("invite-pair"),
    btnCopyInvite: document.getElementById("btn-copy-invite"),
    btnInviteClose: document.getElementById("btn-invite-close"),

    membersModal: document.getElementById("members-modal"),
    memberList: document.getElementById("member-list"),
    membersError: document.getElementById("members-error"),
    btnMembersClose: document.getElementById("btn-members-close"),
    btnRotateInvite: document.getElementById("btn-rotate-invite"),
    btnLeaveTeam: document.getElementById("btn-leave-team"),
  };

  function current() { return currentTeam; }
  function all() { return teams.slice(); }

  function showOnboardingError(msg) {
    if (!el.onboardingError) return;
    el.onboardingError.textContent = msg;
    el.onboardingError.classList.remove("hidden");
  }

  function clearOnboardingError() {
    if (!el.onboardingError) return;
    el.onboardingError.textContent = "";
    el.onboardingError.classList.add("hidden");
  }

  // load fetches the user's teams, keeps the stored selection if still present,
  // and otherwise selects the first team. Zero teams triggers onboarding.
  function load() {
    return api.request("/api/teams").then(function (res) {
      if (!res.ok) return;
      teams = (res.body && res.body.teams) || [];
      renderSwitcher();
      if (teams.length === 0) {
        currentTeam = null;
        window.dispatchEvent(new CustomEvent("cowork:needs-onboarding"));
        return;
      }
      var stored = localStorage.getItem(LS_TEAM);
      var pick = teams.filter(function (t) { return t.id === stored; })[0] || teams[0];
      select(pick.id);
    });
  }

  function select(teamID) {
    var team = teams.filter(function (t) { return t.id === teamID; })[0];
    if (!team) return;
    currentTeam = team;
    localStorage.setItem(LS_TEAM, team.id);
    if (el.switcher) el.switcher.value = team.id;
    window.dispatchEvent(new CustomEvent("cowork:team-changed", { detail: { team: team } }));
  }

  function renderSwitcher() {
    if (!el.switcher) return;
    el.switcher.innerHTML = "";
    teams.forEach(function (t) {
      var opt = document.createElement("option");
      opt.value = t.id;
      opt.textContent = t.name;
      el.switcher.appendChild(opt);
    });
    if (currentTeam) el.switcher.value = currentTeam.id;
  }

  function create(name) {
    return api.request("/api/teams", {
      method: "POST",
      body: JSON.stringify({ name: name })
    }).then(function (res) {
      if (res.ok && res.body && res.body.team) {
        showInviteModal(res.body.team.id, res.body.invite_code);
        return load().then(function () {
          select(res.body.team.id);
          return { ok: true };
        });
      }
      return { ok: false, error: (res.body && res.body.error) || ("创建失败（" + res.status + "）") };
    });
  }

  function join(teamID, inviteCode) {
    return api.request("/api/teams/join", {
      method: "POST",
      body: JSON.stringify({ team_id: teamID, invite_code: inviteCode })
    }).then(function (res) {
      if (res.ok && res.body && res.body.id) {
        return load().then(function () {
          select(res.body.id);
          return { ok: true };
        });
      }
      return { ok: false, error: (res.body && res.body.error) || ("加入失败（" + res.status + "）") };
    });
  }

  // ---- invite modal ------------------------------------------------------

  function applyInvitePair(parsed) {
    if (!parsed) return;
    if (el.inputTeamID) el.inputTeamID.value = parsed.teamID;
    if (el.inputInvite) el.inputInvite.value = parsed.inviteCode;
  }

  function onJoinFieldPaste(ev) {
    var data = ev.clipboardData || window.clipboardData;
    if (!data) return;
    var text = data.getData("text/plain") || data.getData("text") || "";
    var parsed = parseInvitePair(text);
    if (!parsed) return;
    ev.preventDefault();
    applyInvitePair(parsed);
  }

  function showInviteModal(teamID, code) {
    if (!el.inviteModal) return;
    if (el.invitePair) el.invitePair.value = invitePairText(teamID, code);
    if (el.btnCopyInvite) el.btnCopyInvite.textContent = "复制团队 ID 和邀请码";
    el.inviteModal.classList.remove("hidden");
  }

  function hideInviteModal() {
    if (!el.inviteModal) return;
    if (el.invitePair) el.invitePair.value = "";
    el.inviteModal.classList.add("hidden");
  }

  function copyInvitePair() {
    var text = el.invitePair ? el.invitePair.value : "";
    if (!text) return;
    var done = function () {
      el.btnCopyInvite.textContent = "已复制";
      setTimeout(function () { el.btnCopyInvite.textContent = "复制团队 ID 和邀请码"; }, 1500);
    };
    var fallback = function () {
      el.invitePair.focus();
      el.invitePair.select();
      try {
        if (document.execCommand("copy")) done();
      } catch (e) { /* leave the selection so the pair can be copied by hand */ }
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done).catch(fallback);
      return;
    }
    fallback();
  }

  // ---- member administration --------------------------------------------

  function selfID() {
    var user = window.CoworkAuth && window.CoworkAuth.user();
    return user ? user.id : "";
  }

  function showMembersError(msg) {
    if (!el.membersError) return;
    el.membersError.textContent = msg;
    el.membersError.classList.remove("hidden");
  }

  function clearMembersError() {
    if (!el.membersError) return;
    el.membersError.textContent = "";
    el.membersError.classList.add("hidden");
  }

  function openMembers() {
    if (!currentTeam || !el.membersModal) return;
    clearMembersError();
    var canRotate = currentTeam.role === "owner" || currentTeam.role === "admin";
    el.btnRotateInvite.classList.toggle("hidden", !canRotate);
    if (currentTeam.role === "owner") {
      el.btnLeaveTeam.textContent = "离开前请先转让所有权";
    } else {
      el.btnLeaveTeam.textContent = "离开团队";
    }
    el.membersModal.classList.remove("hidden");
    loadMembers();
  }

  function hideMembers() {
    if (el.membersModal) el.membersModal.classList.add("hidden");
  }

  function loadMembers() {
    return api.request("/api/teams/" + encodeURIComponent(currentTeam.id) + "/members").then(function (res) {
      if (!res.ok) {
        showMembersError((res.body && res.body.error) || ("加载成员失败（" + res.status + "）"));
        return;
      }
      renderMembers((res.body && res.body.members) || []);
    });
  }

  function renderMembers(members) {
    el.memberList.innerHTML = "";
    members.forEach(function (m) {
      el.memberList.appendChild(memberRow(m, currentTeam.role));
    });
  }

  function memberRow(member, currentRole) {
    var row = document.createElement("li");
    row.className = "member-row";
    var name = document.createElement("span");
    name.className = "member-name";
    name.textContent = member.username;
    var role = document.createElement("span");
    role.className = "member-role";
    role.textContent = member.role;
    row.appendChild(name);
    row.appendChild(role);
    appendAllowedActions(row, member, currentRole);
    return row;
  }

  function appendAllowedActions(row, member, currentRole) {
    if (member.user_id === selfID()) return;
    var actions = document.createElement("span");
    actions.className = "member-actions";

    if (currentRole === "owner") {
      if (member.role === "member") {
        actions.appendChild(actionButton("设为管理员", function () { changeRole(member, "admin"); }));
      } else if (member.role === "admin") {
        actions.appendChild(actionButton("设为成员", function () { changeRole(member, "member"); }));
      }
      if (member.role !== "owner") {
        actions.appendChild(actionButton("移除", function () { removeMember(member); }, "btn-danger"));
        actions.appendChild(actionButton("转让所有权", function () { transferOwner(member); }));
      }
    } else if (currentRole === "admin" && member.role === "member") {
      actions.appendChild(actionButton("移除", function () { removeMember(member); }, "btn-danger"));
    }

    if (actions.children.length > 0) row.appendChild(actions);
  }

  function actionButton(label, handler, extraClass) {
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn-secondary btn-small" + (extraClass ? " " + extraClass : "");
    btn.textContent = label;
    btn.addEventListener("click", handler);
    return btn;
  }

  function changeRole(member, role) {
    api.request("/api/teams/" + encodeURIComponent(currentTeam.id) + "/members/" + encodeURIComponent(member.user_id), {
      method: "PATCH",
      body: JSON.stringify({ role: role })
    }).then(afterMemberMutation);
  }

  function removeMember(member) {
    if (!window.confirm("确认移除 " + member.username + "？")) return;
    api.request("/api/teams/" + encodeURIComponent(currentTeam.id) + "/members/" + encodeURIComponent(member.user_id), {
      method: "DELETE"
    }).then(afterMemberMutation);
  }

  function transferOwner(member) {
    if (!window.confirm("确认转让给 " + member.username + "？你将成为管理员。")) return;
    api.request("/api/teams/" + encodeURIComponent(currentTeam.id) + "/transfer-owner", {
      method: "POST",
      body: JSON.stringify({ user_id: member.user_id })
    }).then(function (res) {
      if (res.ok) {
        load().then(loadMembers);
      } else {
        showMembersError((res.body && res.body.error) || ("操作失败（" + res.status + "）"));
      }
    });
  }

  function rotateInvite() {
    api.request("/api/teams/" + encodeURIComponent(currentTeam.id) + "/invite/rotate", {
      method: "POST"
    }).then(function (res) {
      if (res.ok && res.body && res.body.invite_code) {
        showInviteModal(currentTeam.id, res.body.invite_code);
      } else {
        showMembersError((res.body && res.body.error) || ("操作失败（" + res.status + "）"));
      }
    });
  }

  function leaveTeam() {
    if (currentTeam.role === "owner") {
      showMembersError("离开前请先转让所有权");
      return;
    }
    if (!window.confirm("确认离开 " + currentTeam.name + "？")) return;
    api.request("/api/teams/" + encodeURIComponent(currentTeam.id) + "/leave", {
      method: "POST"
    }).then(function (res) {
      if (res.ok) {
        localStorage.removeItem(LS_TEAM);
        hideMembers();
        load();
      } else {
        showMembersError((res.body && res.body.error) || ("操作失败（" + res.status + "）"));
      }
    });
  }

  function afterMemberMutation(res) {
    if (res.ok) {
      loadMembers();
    } else {
      showMembersError((res.body && res.body.error) || ("操作失败（" + res.status + "）"));
    }
  }

  // ---- wiring ------------------------------------------------------------

  if (el.switcher) {
    el.switcher.addEventListener("change", function () {
      select(el.switcher.value);
    });
  }

  if (el.formCreate) {
    el.formCreate.addEventListener("submit", function (ev) {
      ev.preventDefault();
      clearOnboardingError();
      create(el.inputName.value.trim()).then(function (res) {
        if (!res.ok) showOnboardingError(res.error);
        else el.formCreate.reset();
      });
    });
  }

  if (el.formJoin) {
    el.formJoin.addEventListener("submit", function (ev) {
      ev.preventDefault();
      clearOnboardingError();
      join(el.inputTeamID.value.trim(), el.inputInvite.value).then(function (res) {
        if (!res.ok) showOnboardingError(res.error);
        else el.formJoin.reset();
      });
    });
  }

  if (el.btnMembersClose) el.btnMembersClose.addEventListener("click", hideMembers);
  if (el.btnRotateInvite) el.btnRotateInvite.addEventListener("click", rotateInvite);
  if (el.btnLeaveTeam) el.btnLeaveTeam.addEventListener("click", leaveTeam);

  if (el.btnInviteClose) el.btnInviteClose.addEventListener("click", hideInviteModal);
  if (el.btnCopyInvite) el.btnCopyInvite.addEventListener("click", copyInvitePair);
  if (el.inputTeamID) el.inputTeamID.addEventListener("paste", onJoinFieldPaste);
  if (el.inputInvite) el.inputInvite.addEventListener("paste", onJoinFieldPaste);

  window.CoworkTeams = {
    load: load,
    create: create,
    join: join,
    select: select,
    current: current,
    all: all,
    openMembers: openMembers,
    parseInvitePair: parseInvitePair,
  };
})(typeof window !== "undefined" ? window : undefined);
