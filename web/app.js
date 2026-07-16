// cowork GUI orchestrator — vanilla JS, no build step by design.
//
// Auth model (v2): username/password accounts with same-origin HttpOnly session
// cookies and a readable CSRF cookie. This module owns page-level view state and
// wires the auth forms; team and dashboard behavior live in their own modules
// (CoworkTeams, CoworkDashboard) and are invoked here only if present.
(function () {
  "use strict";

  var auth = window.CoworkAuth;

  var el = {
    viewAuth: document.getElementById("view-auth"),
    viewOnboarding: document.getElementById("view-onboarding"),
    viewApp: document.getElementById("view-app"),

    formLogin: document.getElementById("form-login"),
    formRegister: document.getElementById("form-register"),
    formChangePassword: document.getElementById("form-change-password"),
    btnShowRegister: document.getElementById("btn-show-register"),
    btnShowLogin: document.getElementById("btn-show-login"),
    authError: document.getElementById("auth-error"),

    loginUsername: document.getElementById("input-login-username"),
    loginPassword: document.getElementById("input-login-password"),
    registerUsername: document.getElementById("input-register-username"),
    registerPassword: document.getElementById("input-register-password"),
    registerConfirm: document.getElementById("input-register-confirm"),
    currentPassword: document.getElementById("input-current-password"),
    newPassword: document.getElementById("input-new-password"),

    currentUsername: document.getElementById("current-username"),
    btnLogout: document.getElementById("btn-logout"),
  };

  // ---- view state --------------------------------------------------------

  function hide(node) { if (node) node.classList.add("hidden"); }
  function show(node) { if (node) node.classList.remove("hidden"); }

  function showOnly(view) {
    [el.viewAuth, el.viewOnboarding, el.viewApp].forEach(hide);
    show(view);
  }

  function showLoginForm() {
    showOnly(el.viewAuth);
    show(el.formLogin);
    hide(el.formRegister);
    hide(el.formChangePassword);
    clearAuthError();
  }

  function showRegisterForm() {
    showOnly(el.viewAuth);
    hide(el.formLogin);
    show(el.formRegister);
    hide(el.formChangePassword);
    clearAuthError();
  }

  function showChangePasswordForm() {
    showOnly(el.viewAuth);
    hide(el.formLogin);
    hide(el.formRegister);
    show(el.formChangePassword);
    clearAuthError();
  }

  function showApp() {
    showOnly(el.viewApp);
  }

  function showOnboarding() {
    showOnly(el.viewOnboarding);
  }

  function clearAuthError() {
    el.authError.textContent = "";
    hide(el.authError);
  }

  function showAuthError(msg) {
    el.authError.textContent = msg;
    show(el.authError);
  }

  // ---- auth wiring -------------------------------------------------------

  el.btnShowRegister.addEventListener("click", showRegisterForm);
  el.btnShowLogin.addEventListener("click", showLoginForm);

  el.formLogin.addEventListener("submit", function (ev) {
    ev.preventDefault();
    clearAuthError();
    auth.login(el.loginUsername.value.trim(), el.loginPassword.value).then(function (res) {
      if (!res.ok) showAuthError(res.error);
      else el.formLogin.reset();
    });
  });

  el.formRegister.addEventListener("submit", function (ev) {
    ev.preventDefault();
    clearAuthError();
    if (el.registerPassword.value !== el.registerConfirm.value) {
      showAuthError("Passwords do not match.");
      return;
    }
    auth.register(el.registerUsername.value.trim(), el.registerPassword.value).then(function (res) {
      if (!res.ok) showAuthError(res.error);
      else el.formRegister.reset();
    });
  });

  el.formChangePassword.addEventListener("submit", function (ev) {
    ev.preventDefault();
    clearAuthError();
    auth.changePassword(el.currentPassword.value, el.newPassword.value).then(function (res) {
      if (!res.ok) {
        showAuthError(res.error);
        return;
      }
      el.formChangePassword.reset();
      showAuthError("Password changed. Please sign in again.");
      showLoginForm();
    });
  });

  el.btnLogout.addEventListener("click", function () {
    auth.logout();
  });

  // ---- orchestration -----------------------------------------------------

  function afterAuthenticated(user) {
    if (el.currentUsername) el.currentUsername.textContent = user ? user.username : "";
    if (window.CoworkTeams) {
      window.CoworkTeams.load();
    } else {
      showApp();
    }
  }

  window.addEventListener("cowork:authenticated", function (ev) {
    afterAuthenticated(ev.detail);
  });

  window.addEventListener("cowork:must-change-password", function () {
    showChangePasswordForm();
    showAuthError("Your password must be changed before continuing.");
  });

  window.addEventListener("cowork:signed-out", function () {
    if (window.CoworkDashboard) window.CoworkDashboard.teardown();
    showLoginForm();
  });

  window.addEventListener("cowork:needs-onboarding", function () {
    showOnboarding();
  });

  window.addEventListener("cowork:team-changed", function () {
    showApp();
  });

  var reauthing = false;
  window.addEventListener("cowork:unauthorized", function () {
    if (reauthing) return;
    reauthing = true;
    auth.bootstrap().then(function (user) {
      reauthing = false;
      if (!user) showLoginForm();
    });
  });

  // ---- boot --------------------------------------------------------------

  showLoginForm();
  auth.bootstrap();
})();
