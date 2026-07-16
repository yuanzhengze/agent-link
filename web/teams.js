// Team onboarding, switching, and (Task 3) member administration for the cowork
// GUI. Only the non-sensitive current team id is persisted; invite codes are
// shown once and never stored.
(function () {
  "use strict";

  var api = window.CoworkAPI;
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
    inviteModalTeam: document.getElementById("invite-modal-team"),
    inviteModalCode: document.getElementById("invite-modal-code"),
    btnCopyInvite: document.getElementById("btn-copy-invite"),
    btnInviteClose: document.getElementById("btn-invite-close"),
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
      return { ok: false, error: (res.body && res.body.error) || ("Create failed (" + res.status + ")") };
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
      return { ok: false, error: (res.body && res.body.error) || ("Join failed (" + res.status + ")") };
    });
  }

  // ---- invite modal ------------------------------------------------------

  function showInviteModal(teamID, code) {
    if (!el.inviteModal) return;
    el.inviteModalTeam.textContent = teamID;
    el.inviteModalCode.textContent = code;
    el.inviteModal.classList.remove("hidden");
  }

  function hideInviteModal() {
    if (!el.inviteModal) return;
    el.inviteModalCode.textContent = "";
    el.inviteModalTeam.textContent = "";
    el.inviteModal.classList.add("hidden");
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

  if (el.btnInviteClose) el.btnInviteClose.addEventListener("click", hideInviteModal);
  if (el.btnCopyInvite) {
    el.btnCopyInvite.addEventListener("click", function () {
      var code = el.inviteModalCode.textContent;
      if (navigator.clipboard && code) {
        navigator.clipboard.writeText(code).then(function () {
          el.btnCopyInvite.textContent = "Copied";
          setTimeout(function () { el.btnCopyInvite.textContent = "Copy"; }, 1500);
        });
      }
    });
  }

  window.CoworkTeams = {
    load: load,
    create: create,
    join: join,
    select: select,
    current: current,
    all: all,
  };
})();
