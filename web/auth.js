// Account authentication for the cowork GUI. Session state lives only in the
// server-issued HttpOnly cookie; this module holds the sanitized user object in
// memory and drives view transitions through custom events.
(function () {
  "use strict";

  var api = window.CoworkAPI;
  var currentUser = null;

  function setUser(user) {
    currentUser = user || null;
  }

  function emitAuthenticated(user) {
    setUser(user);
    if (user && user.must_change_password) {
      window.dispatchEvent(new CustomEvent("cowork:must-change-password", { detail: user }));
      return;
    }
    window.dispatchEvent(new CustomEvent("cowork:authenticated", { detail: user }));
  }

  function emitSignedOut() {
    setUser(null);
    window.dispatchEvent(new CustomEvent("cowork:signed-out"));
  }

  // bootstrap resolves the current session via /api/auth/me, which also rotates
  // the CSRF token. It is safe to call repeatedly (e.g. after a 401).
  function bootstrap() {
    return api.request("/api/auth/me").then(function (res) {
      if (res.ok && res.body && res.body.user) {
        emitAuthenticated(res.body.user);
        return res.body.user;
      }
      emitSignedOut();
      return null;
    });
  }

  function login(username, password) {
    return api.request("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({ username: username, password: password })
    }).then(function (res) {
      if (res.ok && res.body && res.body.user) {
        emitAuthenticated(res.body.user);
        return { ok: true };
      }
      return { ok: false, error: errorText(res) };
    });
  }

  function register(username, password) {
    return api.request("/api/auth/register", {
      method: "POST",
      body: JSON.stringify({ username: username, password: password })
    }).then(function (res) {
      if (res.ok && res.body && res.body.user) {
        emitAuthenticated(res.body.user);
        return { ok: true };
      }
      return { ok: false, error: errorText(res) };
    });
  }

  // changePassword posts new credentials. The server revokes the current
  // session on success, so the caller must return to the login screen.
  function changePassword(currentPassword, newPassword) {
    return api.request("/api/auth/change-password", {
      method: "POST",
      body: JSON.stringify({ current_password: currentPassword, new_password: newPassword })
    }).then(function (res) {
      if (res.ok) {
        emitSignedOut();
        return { ok: true };
      }
      return { ok: false, error: errorText(res) };
    });
  }

  function logout() {
    return api.request("/api/auth/logout", { method: "POST" }).then(function () {
      emitSignedOut();
    });
  }

  function errorText(res) {
    if (res.status === 401) return "Invalid username or password.";
    if (res.status === 409) return "That username is already taken.";
    if (res.status === 429) return "Too many attempts. Please wait a few minutes.";
    if (res.body && res.body.error) return res.body.error;
    return "Request failed (" + res.status + ").";
  }

  window.CoworkAuth = {
    bootstrap: bootstrap,
    login: login,
    register: register,
    changePassword: changePassword,
    logout: logout,
    user: function () { return currentUser; }
  };
})();
