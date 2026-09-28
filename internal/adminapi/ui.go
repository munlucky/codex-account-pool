package adminapi

const adminHTML = `<!doctype html>
<html lang="ko">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>GPT Codex Router 계정 관리</title>
<style>
:root { color-scheme: light dark; font-family: Inter, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
body { margin: 0; background: Canvas; color: CanvasText; }
main { max-width: 1100px; margin: 0 auto; padding: 32px 20px 56px; }
h1 { margin: 0 0 8px; font-size: 28px; }
.sub { margin: 0 0 24px; opacity: .72; }
.card { border: 1px solid color-mix(in srgb, CanvasText 18%, transparent); border-radius: 12px; padding: 18px; margin: 14px 0; }
.row { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
input { min-width: 260px; padding: 10px 12px; border-radius: 8px; border: 1px solid color-mix(in srgb, CanvasText 28%, transparent); background: Canvas; color: CanvasText; }
button { padding: 9px 13px; border-radius: 8px; border: 1px solid color-mix(in srgb, CanvasText 28%, transparent); background: ButtonFace; color: ButtonText; cursor: pointer; }
button:disabled { opacity: .45; cursor: default; }
table { width: 100%; border-collapse: collapse; margin-top: 10px; }
th, td { padding: 10px 8px; border-bottom: 1px solid color-mix(in srgb, CanvasText 14%, transparent); text-align: left; vertical-align: top; }
th { font-size: 12px; opacity: .72; }
code { font-size: 12px; }
.status { font-weight: 650; }
.muted { opacity: .68; }
.warn { color: #b66a00; }
.error { color: #b42318; }
.ok { color: #177245; }
.hidden { display: none; }
#message { min-height: 24px; margin-top: 10px; }
@media (max-width: 760px) { table, thead, tbody, th, td, tr { display: block; } thead { display: none; } td { border-bottom: 0; padding: 5px 0; } tr { padding: 12px 0; border-bottom: 1px solid color-mix(in srgb, CanvasText 14%, transparent); } }
</style>
</head>
<body>
<main>
  <h1>Docker 계정 인증 관리</h1>
  <p class="sub">Codex 계정의 로컬 인증 상태와 실제 upstream 연결 확인 결과를 분리해 표시합니다.</p>

  <section id="loginCard" class="card">
    <h2>관리자 인증</h2>
    <p class="muted">호스트 CLI에서 확인한 관리자 키를 입력하세요. 키는 브라우저 저장소에 보관하지 않습니다.</p>
    <form id="loginForm" class="row">
      <input id="adminKey" type="password" autocomplete="off" placeholder="gcr_admin_…" required>
      <button type="submit">관리 화면 열기</button>
    </form>
  </section>

  <section id="app" class="hidden">
    <div class="card">
      <div class="row">
        <strong>로그인 런타임</strong><span class="ok">Docker 내부 Codex device-code 인증</span>
        <button id="refreshButton" type="button">새로고침</button>
        <button id="logoutButton" type="button">로그아웃</button>
      </div>
    </div>

    <div class="card">
      <h2>계정</h2>
      <div class="row">
        <input id="newProfile" type="text" maxlength="64" placeholder="새 프로필 이름 (예: account-3)">
        <button id="addButton" type="button">새 계정 로그인</button>
      </div>
      <div style="overflow-x:auto">
        <table>
          <thead><tr><th>프로필</th><th>상태</th><th>액세스 만료</th><th>마지막 실제 확인</th><th>작업</th></tr></thead>
          <tbody id="profiles"></tbody>
        </table>
      </div>
      <div id="deviceLoginCard" class="card hidden">
        <strong>ChatGPT 로그인 승인</strong>
        <p class="muted">아래 주소를 브라우저에서 열고 코드를 입력하세요.</p>
        <div class="row">
          <a id="deviceLoginURL" target="_blank" rel="noopener noreferrer"></a>
          <code id="deviceLoginCode"></code>
          <button id="cancelLoginButton" type="button">로그인 취소</button>
        </div>
      </div>
      <div id="message"></div>
    </div>
  </section>
</main>
<script>
(function () {
  var csrf = "";
  var pollTimers = {};
  var activeLoginJobID = "";
  var statusNames = {
    not_logged_in: "로그인 필요",
    unverified: "아직 연결 미확인",
    access_expired_unverified: "액세스 만료 · 갱신 미확인",
    connected: "연결됨",
    reauth_required: "다시 로그인 필요",
    temporarily_unavailable: "일시적으로 확인 불가",
    checking: "연결 확인 중",
    logging_in: "로그인 진행 중"
  };

  function sameOriginHeaders(extra) {
    var headers = extra || {};
    if (csrf) headers["X-CSRF-Token"] = csrf;
    return headers;
  }

  async function request(path, options) {
    options = options || {};
    options.credentials = "same-origin";
    options.headers = sameOriginHeaders(options.headers || {});
    var response = await fetch(path, options);
    if (response.status === 204) return null;
    var data = null;
    try { data = await response.json(); } catch (_) {}
    if (!response.ok) {
      var code = data && data.error && data.error.code ? data.error.code : "request_failed";
      var error = new Error(code);
      error.code = code;
      error.status = response.status;
      throw error;
    }
    return data;
  }

  function setMessage(text, className) {
    var node = document.getElementById("message");
    node.textContent = text || "";
    node.className = className || "";
  }

  function dateText(value) {
    if (!value) return "—";
    try { return new Date(value).toLocaleString(); } catch (_) { return value; }
  }

  function statusClass(status) {
    if (status === "connected") return "ok";
    if (status === "reauth_required" || status === "not_logged_in") return "error";
    if (status === "temporarily_unavailable" || status === "access_expired_unverified") return "warn";
    return "";
  }

  function actionButton(label, disabled, fn) {
    var button = document.createElement("button");
    button.type = "button";
    button.textContent = label;
    button.disabled = disabled;
    button.addEventListener("click", fn);
    return button;
  }

  async function startAction(profile, action) {
    setMessage("");
    try {
      var job = await request("/admin/profiles/" + encodeURIComponent(profile.provider) + "/" + encodeURIComponent(profile.id) + "/" + action, {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: "{}"
      });
      if (action === "login") activeLoginJobID = job.id;
      setMessage((action === "login" ? "Docker 내부 Codex 로그인 준비 중입니다. " : "연결 확인을 시작했습니다. ") + "작업 " + job.id, "");
      watchJob(job.id);
      await loadProfiles();
    } catch (error) {
      setMessage("작업을 시작하지 못했습니다: " + error.code, "error");
    }
  }

  function showLoginChallenge(job) {
    if (!job || !job.verification_url || !job.user_code) return;
    activeLoginJobID = job.id;
    var card = document.getElementById("deviceLoginCard");
    var link = document.getElementById("deviceLoginURL");
    link.href = job.verification_url;
    link.textContent = job.verification_url;
    document.getElementById("deviceLoginCode").textContent = job.user_code;
    card.classList.remove("hidden");
  }

  async function watchJob(id) {
    if (pollTimers[id]) window.clearTimeout(pollTimers[id]);
    try {
      var job = await request("/admin/jobs/" + encodeURIComponent(id));
      if (job.state === "logging_in" && job.verification_url && job.user_code) {
        showLoginChallenge(job);
        setMessage("ChatGPT에서 코드를 승인하면 자동으로 연결 확인까지 진행됩니다.", "");
      }
      if (job.state === "queued" || job.state === "checking" || job.state === "logging_in") {
        pollTimers[id] = window.setTimeout(function () { watchJob(id); }, 1000);
      } else {
        delete pollTimers[id];
        if (activeLoginJobID === id) {
          activeLoginJobID = "";
          document.getElementById("deviceLoginCard").classList.add("hidden");
        }
        var suffix = job.error_code ? " (" + job.error_code + ")" : "";
        setMessage("작업 " + id + ": " + job.state + suffix, job.state === "succeeded" ? "ok" : "warn");
        await loadProfiles();
      }
    } catch (error) {
      delete pollTimers[id];
      setMessage("작업 상태 조회 실패: " + error.code, "error");
    }
  }

  function renderProfiles(data) {
    var body = document.getElementById("profiles");
    body.textContent = "";
    data.profiles.forEach(function (profile) {
      var tr = document.createElement("tr");

      var name = document.createElement("td");
      var title = document.createElement("strong");
      title.textContent = profile.id + (profile.active ? " · 선택됨" : "");
      name.appendChild(title);
      var provider = document.createElement("div");
      provider.className = "muted";
      provider.textContent = profile.provider;
      name.appendChild(provider);

      var state = document.createElement("td");
      var stateText = document.createElement("span");
      stateText.className = "status " + statusClass(profile.status);
      stateText.textContent = statusNames[profile.status] || profile.status;
      state.appendChild(stateText);
      if (profile.error_code) {
        var error = document.createElement("div");
        error.className = "muted";
        error.textContent = profile.error_code;
        state.appendChild(error);
      }

      var expires = document.createElement("td");
      expires.textContent = dateText(profile.access_expires_at);

      var checked = document.createElement("td");
      checked.textContent = dateText(profile.last_checked_at);

      var actions = document.createElement("td");
      var busy = profile.status === "checking" || profile.status === "logging_in";
      actions.appendChild(actionButton("연결 확인", busy, function () { startAction(profile, "check"); }));
      actions.appendChild(document.createTextNode(" "));
      actions.appendChild(actionButton(profile.status === "not_logged_in" ? "로그인" : "다시 로그인", busy, function () { startAction(profile, "login"); }));
      if (busy && profile.job_id) {
        actions.appendChild(document.createTextNode(" "));
        actions.appendChild(actionButton("취소", false, async function () {
          try {
            await request("/admin/jobs/" + encodeURIComponent(profile.job_id) + "/cancel", {
              method: "POST",
              headers: {"Content-Type": "application/json"},
              body: "{}"
            });
            setMessage("작업 취소를 요청했습니다.", "warn");
            await loadProfiles();
          } catch (error) {
            setMessage("취소 실패: " + error.code, "error");
          }
        }));
      }

      tr.appendChild(name);
      tr.appendChild(state);
      tr.appendChild(expires);
      tr.appendChild(checked);
      tr.appendChild(actions);
      body.appendChild(tr);
    });

  }

  async function loadProfiles() {
    var data = await request("/admin/profiles");
    renderProfiles(data);
    if (!activeLoginJobID) {
      var jobs = data.active_jobs || [];
      for (var i = 0; i < jobs.length; i++) {
        if (jobs[i].type === "login" && jobs[i].state === "logging_in") {
          activeLoginJobID = jobs[i].id;
          showLoginChallenge(jobs[i]);
          watchJob(jobs[i].id);
          break;
        }
      }
    }
  }

  async function restoreSession() {
    try {
      var session = await request("/admin/session");
      csrf = session.csrf_token;
      document.getElementById("loginCard").classList.add("hidden");
      document.getElementById("app").classList.remove("hidden");
      await loadProfiles();
    } catch (_) {}
  }

  document.getElementById("loginForm").addEventListener("submit", async function (event) {
    event.preventDefault();
    var input = document.getElementById("adminKey");
    try {
      var session = await request("/admin/session", {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({key: input.value})
      });
      input.value = "";
      csrf = session.csrf_token;
      document.getElementById("loginCard").classList.add("hidden");
      document.getElementById("app").classList.remove("hidden");
      await loadProfiles();
    } catch (error) {
      input.value = "";
      setMessage("관리자 인증 실패: " + error.code, "error");
    }
  });

  document.getElementById("refreshButton").addEventListener("click", function () {
    loadProfiles().catch(function (error) { setMessage("조회 실패: " + error.code, "error"); });
  });

  document.getElementById("addButton").addEventListener("click", async function () {
    var input = document.getElementById("newProfile");
    var id = input.value.trim();
    if (!id) return;
    var profile = {provider: "codex", id: id};
    await startAction(profile, "login");
    input.value = "";
  });

  document.getElementById("cancelLoginButton").addEventListener("click", async function () {
    if (!activeLoginJobID) return;
    try {
      await request("/admin/jobs/" + encodeURIComponent(activeLoginJobID) + "/cancel", {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: "{}"
      });
      setMessage("로그인 취소를 요청했습니다.", "warn");
      document.getElementById("deviceLoginCard").classList.add("hidden");
    } catch (error) {
      setMessage("로그인 취소 실패: " + error.code, "error");
    }
  });

  document.getElementById("logoutButton").addEventListener("click", async function () {
    try { await request("/admin/session", {method: "DELETE"}); } catch (_) {}
    csrf = "";
    document.getElementById("app").classList.add("hidden");
    document.getElementById("loginCard").classList.remove("hidden");
  });

  restoreSession();
})();
</script>
</body>
</html>
`
