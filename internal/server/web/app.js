"use strict";

const state = {
  token: sessionStorage.getItem("01agent_api_token") || "",
  health: null,
  ready: null,
  tasks: [],
  agents: [],
  computers: {},
  bindings: {},
  commands: {},
  automations: [],
  dispatches: [],
};

const titles = {
  overview: "系统总览",
  tasks: "交付任务",
  dispatches: "Dispatcher",
  agents: "Agent 组织",
  computers: "执行节点",
  automations: "定时自动化",
  console: "API Console",
};

const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => Array.from(document.querySelectorAll(selector));

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = String(text);
  return node;
}

function relativeURL(route) {
  return new URL(String(route).replace(/^\/+/, ""), document.baseURI);
}

async function request(route, options = {}, authenticated = true) {
  const headers = new Headers(options.headers || {});
  if (authenticated && state.token) headers.set("Authorization", `Bearer ${state.token}`);
  if (options.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  const response = await fetch(relativeURL(route), { ...options, headers });
  const text = await response.text();
  let body;
  try { body = text ? JSON.parse(text) : null; } catch { body = text; }
  if (!response.ok) {
    const error = new Error(body?.error || body?.reason || `HTTP ${response.status}`);
    error.status = response.status;
    error.body = body;
    throw error;
  }
  return { status: response.status, body };
}

function badge(status) {
  const value = status || "unknown";
  return element("span", `badge ${value}`, value);
}

function formatTime(value) {
  if (!value || String(value).startsWith("0001-")) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString("zh-CN", { hour12: false });
}

function short(value, size = 12) {
  const text = value || "—";
  return text.length > size ? `${text.slice(0, size)}…` : text;
}

function showToast(message) {
  const toast = $("#toast");
  toast.textContent = message;
  toast.classList.add("visible");
  clearTimeout(showToast.timer);
  showToast.timer = setTimeout(() => toast.classList.remove("visible"), 2600);
}

function showDetail(title, value) {
  $("#detail-title").textContent = title;
  $("#detail-json").textContent = JSON.stringify(value, null, 2);
  $("#detail-dialog").showModal();
}

function switchView(view) {
  $$(".nav-item").forEach((item) => item.classList.toggle("active", item.dataset.view === view));
  $$(".view").forEach((item) => item.classList.toggle("active", item.id === `view-${view}`));
  $("#page-title").textContent = titles[view] || titles.overview;
}

async function loadPublicStatus() {
  try {
    state.health = (await request("healthz", {}, false)).body;
    $("#service-dot").className = "ok";
    $("#service-label").textContent = "在线";
    $("#service-version").textContent = state.health.version || "dev";
  } catch (error) {
    $("#service-dot").className = "bad";
    $("#service-label").textContent = "离线";
    $("#service-version").textContent = "—";
  }
  try { state.ready = (await request("readyz", {}, false)).body; }
  catch (error) { state.ready = error.body || { status: "not_ready" }; }
}

async function loadControlPlane() {
  if (!state.token) {
    renderAll();
    return;
  }
  const results = await Promise.allSettled([
    request("v2/tasks"),
    request("v2/agents"),
    request("v2/computers"),
    request("v2/automations"),
    request("v2/dispatches"),
  ]);
  const unauthorized = results.find((result) => result.status === "rejected" && result.reason?.status === 401);
  if (unauthorized) throw unauthorized.reason;
  if (results[0].status === "fulfilled") state.tasks = results[0].value.body.tasks || [];
  if (results[1].status === "fulfilled") state.agents = results[1].value.body.agents || [];
  if (results[2].status === "fulfilled") {
    state.computers = results[2].value.body.computers || {};
    state.bindings = results[2].value.body.bindings || {};
    state.commands = results[2].value.body.commands || {};
  }
  if (results[3].status === "fulfilled") state.automations = results[3].value.body.automations || [];
  if (results[4].status === "fulfilled") state.dispatches = results[4].value.body.executions || [];
  renderAll();
}

async function refresh() {
  const button = $("#refresh-button");
  button.classList.add("spinning");
  await loadPublicStatus();
  try {
    await loadControlPlane();
    if (state.token) $("#auth-panel").classList.add("connected");
  } catch (error) {
    $("#auth-panel").classList.remove("connected");
    $("#auth-error").textContent = error.status === 401 ? "Token 无效，请重新输入。" : error.message;
    if (error.status === 401) {
      state.token = "";
      sessionStorage.removeItem("01agent_api_token");
    }
    renderAll();
  } finally {
    button.classList.remove("spinning");
  }
}

function renderMetrics() {
  const definitions = [
    ["Product Tasks", state.tasks.length, `${state.tasks.filter((item) => item.state === "in_progress").length} 正在执行`],
    ["Persistent Agents", state.agents.length, `${state.agents.reduce((sum, item) => sum + (item.sessions?.length || 0), 0)} session generations`],
    ["Computers", Object.keys(state.computers).length, `${Object.values(state.computers).filter((item) => item.status === "online").length} online`],
    ["Automations", state.automations.length, `${state.automations.filter((item) => item.status === "active").length} active`],
    ["Dispatches", state.dispatches.length, `${state.dispatches.filter((item) => !["succeeded", "failed", "canceled"].includes(item.phase)).length} active`],
  ];
  const root = $("#metric-grid");
  root.replaceChildren(...definitions.map(([label, value, foot]) => {
    const card = element("article", "metric");
    card.append(element("span", "metric-label", label), element("strong", "metric-value", value), element("div", "metric-foot", foot));
    return card;
  }));
}

function renderTaskSummary() {
  const states = ["todo", "in_progress", "in_review", "done", "closed"];
  const total = Math.max(state.tasks.length, 1);
  const root = $("#task-summary");
  if (!state.tasks.length) { root.replaceChildren(element("div", "empty", state.token ? "暂无任务" : "连接后读取任务")); return; }
  root.replaceChildren(...states.map((name) => {
    const count = state.tasks.filter((item) => item.state === name).length;
    const row = element("div", "status-row");
    const progress = element("div", "progress");
    const bar = element("span");
    bar.style.width = `${Math.round(count / total * 100)}%`;
    progress.append(bar);
    row.append(element("span", "status-name", name), progress, element("span", "status-number", count));
    return row;
  }));
}

function renderComputerSummary() {
  const items = Object.values(state.computers).slice(0, 5);
  const root = $("#computer-summary");
  if (!items.length) { root.replaceChildren(element("div", "empty", state.token ? "暂无执行节点" : "连接后读取节点")); return; }
  root.replaceChildren(...items.map((item) => {
    const row = element("div", "node-row");
    const main = element("div", "node-main");
    const capability = item.capabilities?.find((entry) => entry.digest === item.current_capability_digest) || item.capabilities?.at(-1);
    main.append(element("strong", "", item.name || item.id), element("small", "", capability ? `${capability.os}/${capability.arch} · ${capability.runtime}` : item.id));
    row.append(main, badge(item.status));
    return row;
  }));
}

function renderAutomationSummary() {
  const items = [...state.automations].sort((a, b) => new Date(a.next_run_at) - new Date(b.next_run_at)).slice(0, 5);
  const root = $("#automation-summary");
  if (!items.length) { root.replaceChildren(element("div", "empty", state.token ? "暂无自动化" : "连接后读取计划")); return; }
  root.replaceChildren(...items.map((item) => {
    const row = element("div", "timeline-row");
    const main = element("div", "timeline-main");
    main.append(element("strong", "", item.name), element("small", "", `下次 ${formatTime(item.next_run_at)} · ${item.runs?.length || 0} runs`));
    row.append(main, badge(item.status));
    row.addEventListener("click", () => showDetail(item.name, item));
    return row;
  }));
}

function renderTasks() {
  $("#tasks-count").textContent = `${state.tasks.length} items`;
  const root = $("#tasks-table");
  if (!state.tasks.length) {
    const row = element("tr"); const cell = element("td", "empty", state.token ? "暂无任务" : "请输入 Token"); cell.colSpan = 5; row.append(cell); root.replaceChildren(row); return;
  }
  root.replaceChildren(...state.tasks.map((item) => {
    const row = element("tr");
    const title = element("td", "primary-cell"); title.append(element("strong", "", item.title), element("small", "", item.id));
    const status = element("td"); status.append(badge(item.state));
    row.append(title, status, element("td", "", item.assignee_id || "—"), element("td", "", `r${item.revision}`), element("td", "", formatTime(item.updated_at)));
    row.addEventListener("click", () => showDetail(item.title, item));
    return row;
  }));
}

function renderAgents() {
  const root = $("#agents-grid");
  if (!state.agents.length) { root.replaceChildren(element("div", "panel empty", state.token ? "暂无 Agent" : "请输入 Token")); return; }
  root.replaceChildren(...state.agents.map((item) => {
    const card = element("article", "entity-card");
    const top = element("div", "entity-top"); top.append(element("h3", "", item.name), badge("active"));
    const meta = element("div", "entity-meta");
    [["MODEL", item.agent_revisions?.find((entry) => entry.id === item.current_agent_revision_id)?.model || "—"], ["SESSION", `generation ${item.current_session_generation}`], ["AGENT REV", short(item.current_agent_revision_id)], ["RELATIONSHIP", short(item.current_relationship_revision_id)]].forEach(([label, value]) => { const block = element("div"); block.append(element("span", "", label), element("strong", "", value)); meta.append(block); });
    card.append(top, element("p", "entity-id", item.id), meta);
    card.addEventListener("click", () => showDetail(item.name, item));
    return card;
  }));
}

function renderComputers() {
  const items = Object.values(state.computers);
  $("#computers-count").textContent = `${items.length} nodes`;
  const root = $("#computers-table");
  if (!items.length) { const row = element("tr"); const cell = element("td", "empty", state.token ? "暂无 Computer" : "请输入 Token"); cell.colSpan = 5; row.append(cell); root.replaceChildren(row); return; }
  root.replaceChildren(...items.map((item) => {
    const current = item.capabilities?.find((entry) => entry.digest === item.current_capability_digest) || item.capabilities?.at(-1);
    const row = element("tr"); const name = element("td", "primary-cell"); name.append(element("strong", "", item.name), element("small", "", item.id));
    const status = element("td"); status.append(badge(item.status));
    row.append(name, status, element("td", "", current ? `${current.os}/${current.arch}` : "—"), element("td", "", current ? `r${current.revision}` : "—"), element("td", "", formatTime(item.lease_expires_at)));
    row.addEventListener("click", () => showDetail(item.name, item));
    return row;
  }));
}

function renderAutomations() {
  const root = $("#automations-grid");
  if (!state.automations.length) { root.replaceChildren(element("div", "panel empty", state.token ? "暂无 Automation" : "请输入 Token")); return; }
  root.replaceChildren(...state.automations.map((item) => {
    const card = element("article", "entity-card");
    const top = element("div", "entity-top"); top.append(element("h3", "", item.name), badge(item.status));
    const meta = element("div", "entity-meta");
    [["INTERVAL", `${item.every_seconds}s`], ["NEXT RUN", formatTime(item.next_run_at)], ["RUNS", item.runs?.length || 0], ["FAILURES", `${item.consecutive_failures}/${item.failure_pause_threshold}`]].forEach(([label, value]) => { const block = element("div"); block.append(element("span", "", label), element("strong", "", value)); meta.append(block); });
    card.append(top, element("p", "entity-id", item.id), meta);
    card.addEventListener("click", () => showDetail(item.name, item));
    return card;
  }));
}

function renderDispatches() {
  $("#dispatches-count").textContent = `${state.dispatches.length} runs`;
  const root = $("#dispatches-table");
  if (!state.dispatches.length) {
    const row = element("tr"); const cell = element("td", "empty", state.token ? "暂无 Dispatcher 执行" : "请输入 Token"); cell.colSpan = 5; row.append(cell); root.replaceChildren(row); return;
  }
  root.replaceChildren(...state.dispatches.map((item) => {
    const row = element("tr");
    const identity = element("td", "primary-cell"); identity.append(element("strong", "", item.id), element("small", "", `task ${item.task_id}`));
    const phase = element("td"); phase.append(badge(item.phase));
    const actors = element("td", "primary-cell"); actors.append(element("strong", "", item.agent_id), element("small", "", `review: ${item.reviewer_id}`));
    const attempts = element("td", "", `${item.attempt}/${item.max_attempts} · review ${item.review_attempt}`);
    const actions = element("td", "row-actions");
    if (item.phase === "awaiting_human") {
      const pass = element("button", "mini-button success", "人工通过");
      const reject = element("button", "mini-button danger", "退回");
      pass.addEventListener("click", (event) => { event.stopPropagation(); decideHumanGate(item, "pass"); });
      reject.addEventListener("click", (event) => { event.stopPropagation(); decideHumanGate(item, "reject"); });
      actions.append(pass, reject);
    }
    if (!["succeeded", "failed", "canceled", "canceling"].includes(item.phase)) {
      const cancel = element("button", "mini-button", "取消");
      cancel.addEventListener("click", (event) => { event.stopPropagation(); cancelDispatch(item); });
      actions.append(cancel);
    }
    if (!actions.childElementCount) actions.textContent = "—";
    row.append(identity, phase, actors, attempts, actions);
    row.addEventListener("click", () => showDetail(item.id, item));
    return row;
  }));
}

async function cancelDispatch(execution) {
  const reason = window.prompt("取消原因", "用户取消")?.trim();
  if (!reason) return;
  try {
    await request(`v2/dispatches/${encodeURIComponent(execution.id)}/cancel`, { method: "POST", body: JSON.stringify({ operation_id: `ui-cancel-${execution.id}-${Date.now()}`, reason }) });
    showToast("已提交取消"); await loadControlPlane();
  } catch (error) { showToast(`取消失败：${error.message}`); }
}

async function decideHumanGate(execution, decision) {
  const task = state.tasks.find((item) => item.id === execution.task_id);
  const latest = task?.submissions?.at(-1);
  if (!task || !latest) { showToast("缺少 Task handoff，无法审核"); return; }
  const reason = window.prompt(decision === "pass" ? "通过理由" : "退回理由", decision === "pass" ? "人工核验通过" : "人工核验未通过")?.trim();
  if (!reason) return;
  const body = { action: "review", operation_id: `ui-human-${decision}-${execution.id}-${Date.now()}`, expected_revision: task.revision, gate_result: { decision, reviewer_id: execution.reviewer_id, artifact_versions: latest.artifacts || [], evidence: ["manual gate decision from control plane"], reason } };
  try {
    await request(`v2/tasks/${encodeURIComponent(task.id)}/actions`, { method: "POST", body: JSON.stringify(body) });
    await request("v2/dispatches/reconcile", { method: "POST" });
    showToast(decision === "pass" ? "人工 Gate 已通过" : "已退回执行者"); await loadControlPlane();
  } catch (error) { showToast(`Gate 操作失败：${error.message}`); }
}

function renderAll() {
  renderMetrics();
  renderTaskSummary();
  renderComputerSummary();
  renderAutomationSummary();
  renderTasks();
  renderAgents();
  renderComputers();
  renderAutomations();
  renderDispatches();
}

$("#token-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  state.token = $("#token-input").value.trim();
  $("#auth-error").textContent = "";
  try {
    await loadControlPlane();
    sessionStorage.setItem("01agent_api_token", state.token);
    $("#token-input").value = "";
    $("#auth-panel").classList.add("connected");
    showToast("控制平面已连接");
  } catch (error) {
    state.token = "";
    sessionStorage.removeItem("01agent_api_token");
    $("#auth-error").textContent = error.status === 401 ? "Token 无效。" : error.message;
  }
});

$("#refresh-button").addEventListener("click", refresh);
$("#auth-button").addEventListener("click", () => {
  state.token = "";
  state.tasks = [];
  state.agents = [];
  state.computers = {};
  state.bindings = {};
  state.commands = {};
  state.automations = [];
  state.dispatches = [];
  sessionStorage.removeItem("01agent_api_token");
  $("#auth-panel").classList.remove("connected");
  $("#auth-error").textContent = "";
  renderAll();
  showToast("当前会话 Token 已清除");
});
$("#detail-close").addEventListener("click", () => $("#detail-dialog").close());
$$('[data-view]').forEach((item) => item.addEventListener("click", () => switchView(item.dataset.view)));
$$('[data-go]').forEach((item) => item.addEventListener("click", () => switchView(item.dataset.go)));

$("#task-create-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const id = String(form.get("id") || "").trim();
  const payload = {
    operation_id: `ui-create-${id}`, id, workspace_id: "default", channel_id: "control-plane",
    parent_task_id: String(form.get("parent") || "").trim(), creator_id: "human", title: String(form.get("title") || "").trim(), objective: String(form.get("objective") || "").trim(),
    requirements: [{ id: "R1", text: String(form.get("requirement") || "").trim() }], scope: { allow: [String(form.get("scope") || ".").trim()] }, stop_conditions: ["R1 verified"],
    assignee_id: String(form.get("assignee") || "").trim(), gate: { kind: "agent", reviewer_id: String(form.get("reviewer") || "").trim(), checks: ["verify R1 against the artifact"], required_evidence: ["run artifact"], on_reject: "return to assignee" },
  };
  const status = $("#task-create-status"); status.textContent = "创建中…";
  try { await request("v2/tasks", { method: "POST", body: JSON.stringify(payload) }); status.textContent = "已创建"; event.currentTarget.reset(); event.currentTarget.elements.scope.value = "."; await loadControlPlane(); }
  catch (error) { status.textContent = error.message; }
});

$("#dispatch-create-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const id = String(form.get("id") || "").trim();
  const payload = { operation_id: `ui-start-${id}`, id, task_id: String(form.get("task") || "").trim(), max_attempts: Number(form.get("attempts")), timeout_seconds: Number(form.get("timeout")) };
  const status = $("#dispatch-create-status"); status.textContent = "启动中…";
  try { await request("v2/dispatches", { method: "POST", body: JSON.stringify(payload) }); status.textContent = "已启动"; await loadControlPlane(); }
  catch (error) { status.textContent = error.message; }
});

$("#reconcile-dispatches").addEventListener("click", async () => {
  try { await request("v2/dispatches/reconcile", { method: "POST" }); showToast("已执行一次调度"); await loadControlPlane(); }
  catch (error) { showToast(`调度失败：${error.message}`); }
});

$("#api-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const method = $("#api-method").value;
  const route = $("#api-path").value.trim().replace(/^\/+/, "");
  const rawBody = $("#api-body").value.trim();
  const options = { method };
  if (rawBody) {
    try { options.body = JSON.stringify(JSON.parse(rawBody)); }
    catch (error) { $("#api-status").textContent = "JSON 无效"; $("#api-response").textContent = error.message; return; }
  }
  const started = performance.now();
  $("#api-status").textContent = "请求中…";
  try {
    const result = await request(route, options);
    $("#api-status").textContent = `${result.status} OK`;
    $("#api-response").textContent = JSON.stringify(result.body, null, 2);
  } catch (error) {
    $("#api-status").textContent = `${error.status || "ERR"} ${error.message}`;
    $("#api-response").textContent = JSON.stringify(error.body || { error: error.message }, null, 2);
  } finally {
    $("#api-duration").textContent = `${Math.round(performance.now() - started)} ms`;
  }
});

renderAll();
refresh();
setInterval(() => { if (state.token && !document.hidden) loadControlPlane().catch(() => {}); }, 5000);
