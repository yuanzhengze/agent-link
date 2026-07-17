// Same-origin Cookie/CSRF fetch wrapper for the cowork GUI. No credential is
// ever read from or written to localStorage, the DOM, or the URL: the browser
// carries the HttpOnly al_session cookie automatically, and the readable
// al_csrf cookie is echoed back in X-CSRF-Token on mutating requests.
(function () {
  "use strict";

  function cookie(name) {
    var prefix = name + "=";
    return document.cookie
      .split(";")
      .map(function (v) { return v.trim(); })
      .filter(function (v) { return v.indexOf(prefix) === 0; })
      .map(function (v) { return decodeURIComponent(v.slice(prefix.length)); })[0] || "";
  }

  function request(path, options) {
    options = options || {};
    var method = (options.method || "GET").toUpperCase();
    var headers = Object.assign({}, options.headers || {});
    if (options.body && !headers["Content-Type"]) {
      headers["Content-Type"] = "application/json";
    }
    if (["POST", "PATCH", "DELETE"].indexOf(method) >= 0) {
      headers["X-CSRF-Token"] = cookie("al_csrf");
    }
    return fetch(path, Object.assign({}, options, {
      method: method,
      headers: headers,
      credentials: "same-origin"
    })).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (body) {
        if (response.status === 401) {
          window.dispatchEvent(new CustomEvent("cowork:unauthorized"));
        }
        return { ok: response.ok, status: response.status, body: body };
      });
    }).catch(function (err) {
      return { ok: false, status: 0, body: { error: (err && err.message) || "network error" } };
    });
  }

  window.CoworkAPI = { request: request, cookie: cookie };
})();
