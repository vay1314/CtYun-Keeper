document.addEventListener("submit", (event) => {
  const message = event.target.dataset.confirm;
  if (message && !window.confirm(message)) event.preventDefault();
});

const themeStorageKey = "ctyun-theme";
const syncThemeButton = () => {
  const button = document.querySelector("[data-theme-toggle]");
  if (!button) return;
  const dark = document.documentElement.dataset.theme === "dark";
  const label = dark ? "切换到白天模式" : "切换到夜间模式";
  button.setAttribute("aria-label", label);
  button.setAttribute("title", label);
};

document.addEventListener("click", (event) => {
  const button = event.target.closest("[data-theme-toggle]");
  if (!button) return;
  const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = next;
  try {
    localStorage.setItem(themeStorageKey, next);
  } catch (_) {
    // 浏览器禁用本地存储时，主题仍对当前页面有效。
  }
  syncThemeButton();
});

document.addEventListener("click", (event) => {
  const button = event.target.closest("[data-password-toggle]");
  if (!button) return;
  const input = button.closest(".password-field")?.querySelector("input");
  if (!input) return;
  const show = input.type === "password";
  input.type = show ? "text" : "password";
  button.setAttribute("aria-pressed", String(show));
  button.setAttribute("aria-label", show ? "隐藏密码" : "显示密码");
  input.focus();
});

const syncKeepalivePeriod = (select) => {
  const config = select?.closest(".keepalive-config");
  const period = config?.querySelector("[data-keepalive-period]");
  if (period) period.hidden = select.value !== "scheduled";
  config?.querySelectorAll("[data-keepalive-hint]").forEach((hint) => {
    hint.hidden = hint.dataset.keepaliveHint !== select.value;
  });
};

document.addEventListener("change", (event) => {
  if (event.target.matches("[data-keepalive-mode]")) syncKeepalivePeriod(event.target);
});

document.addEventListener("DOMContentLoaded", () => {
  syncThemeButton();
  document.querySelectorAll("[data-keepalive-mode]").forEach(syncKeepalivePeriod);
  const toggle = document.querySelector(".sidebar-toggle");
  const backdrop = document.querySelector(".sidebar-backdrop");
  const closeSidebar = () => {
    document.body.classList.remove("sidebar-open");
    toggle?.setAttribute("aria-expanded", "false");
  };
  toggle?.addEventListener("click", () => {
    if (window.matchMedia("(max-width: 760px)").matches) {
      document.body.classList.toggle("sidebar-open");
      toggle.setAttribute(
        "aria-expanded",
        document.body.classList.contains("sidebar-open") ? "true" : "false",
      );
    } else {
      document.body.classList.toggle("sidebar-collapsed");
    }
  });
  backdrop?.addEventListener("click", closeSidebar);
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") closeSidebar();
  });

  const uptime = document.querySelector("#program-uptime[data-uptime-seconds]");
  if (uptime) {
    const initialSeconds = Math.max(0, Number(uptime.dataset.uptimeSeconds) || 0);
    const startedCountingAt = performance.now();
    const formatUptime = (value) => {
      const totalSeconds = Math.max(0, Math.floor(value));
      const days = Math.floor(totalSeconds / 86400);
      const hours = Math.floor((totalSeconds % 86400) / 3600);
      const minutes = Math.floor((totalSeconds % 3600) / 60);
      const seconds = totalSeconds % 60;
      const clock = [hours, minutes, seconds]
        .map((part) => String(part).padStart(2, "0"))
        .join(":");
      return days ? `${days}天 ${clock}` : clock;
    };
    const updateUptime = () => {
      const elapsed = (performance.now() - startedCountingAt) / 1000;
      uptime.textContent = formatUptime(initialSeconds + elapsed);
    };
    updateUptime();
    window.setInterval(updateUptime, 1000);
  }

  const output = document.querySelector("#log-output[data-stream]");
  if (!output) return;
  const streamUrl = new URL(output.dataset.stream, window.location.origin);
  streamUrl.searchParams.set("offset", output.dataset.offset || "0");
  const stream = new EventSource(streamUrl);
  stream.onmessage = (event) => {
    if (output.textContent === "暂无日志输出。") output.textContent = "";
    output.textContent = JSON.parse(event.data) + output.textContent;
    output.scrollTop = 0;
  };
  stream.addEventListener("reset", () => {
    output.textContent = "";
    output.scrollTop = 0;
  });
  stream.addEventListener("done", () => stream.close());
});

(() => {
  const storageKey = "ctyun-log-sources-scroll";
  let position = { top: 0, left: 0 };

  const readPosition = (sources) => ({ top: sources.scrollTop, left: sources.scrollLeft });
  const restorePosition = (sources) => {
    if (!sources) return;
    sources.scrollTop = position.top;
    sources.scrollLeft = position.left;
  };

  const sources = document.getElementById("log-sources");
  if (!sources) return;
  try {
    const saved = JSON.parse(sessionStorage.getItem(storageKey) || "null");
    if (Number.isFinite(saved?.top) && Number.isFinite(saved?.left)) position = saved;
    sessionStorage.removeItem(storageKey);
  } catch (_) {}
  restorePosition(sources);

  document.addEventListener("click", (event) => {
    if (!(event.target instanceof Element)) return;
    const link = event.target.closest("#log-sources a");
    if (!link) return;
    position = readPosition(link.closest("#log-sources"));
    try {
      sessionStorage.setItem(storageKey, JSON.stringify(position));
    } catch (_) {}
  });

  document.addEventListener("htmx:beforeSwap", (event) => {
    if (event.detail.target?.id === "log-sources") position = readPosition(event.detail.target);
  });
  document.addEventListener("htmx:afterSwap", (event) => {
    if (event.detail.target?.id === "log-sources") restorePosition(document.getElementById("log-sources"));
  });
})();

(() => {
  if (document.body.dataset.authenticated !== "true") return;
  const context = document.modelContext;
  if (!context?.registerTool) return;
  const csrf = document.querySelector('meta[name="csrf-token"]')?.content || "";

  context.registerTool({
    name: "get_ctyun_keeper_status",
    title: "读取 CtYunKeeper 状态",
    description: "读取 CtYun 保活、账号数量和当前任务状态。",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
    annotations: { readOnlyHint: true, untrustedContentHint: false },
    async execute() {
      const response = await fetch("/api/status");
      if (!response.ok) throw new Error("无法读取运行状态");
      return response.json();
    },
  });

  context.registerTool({
    name: "run_ctyun_account_task",
    title: "运行账号任务",
    description: "为指定账号启动登录云电脑、AI 对话或云电脑挂机任务。",
    inputSchema: {
      type: "object",
      properties: {
        accountId: { type: "integer", minimum: 1 },
        taskType: { type: "string", enum: ["login", "chat", "pc"] },
      },
      required: ["accountId", "taskType"],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: false, untrustedContentHint: false },
    async execute(input) {
      if (!Number.isInteger(input.accountId) || !["login", "chat", "pc"].includes(input.taskType)) {
        throw new Error("账号编号或任务类型无效");
      }
      const response = await fetch(`/api/accounts/${input.accountId}/tasks/${input.taskType}`, {
        method: "POST",
        headers: { "x-csrf-token": csrf },
      });
      const result = await response.json();
      if (!response.ok) throw new Error(result.error || "任务启动失败");
      window.location.assign(`/logs?run_id=${result.run_id}`);
      return result;
    },
  });
})();

const seenUpdateMessages = new Set();

const waitForUpdateMessage = (milliseconds) =>
  new Promise((resolve) => window.setTimeout(resolve, milliseconds));

const showUpdateMessage = async (message) => {
  const queue = document.querySelector("[data-update-message-queue]");
  if (!queue) return;
  const item = document.createElement("div");
  item.className = `update-status ${message.level}`;
  item.setAttribute("role", message.level === "error" ? "alert" : "status");
  item.textContent = message.text;
  queue.append(item);
  await new Promise((resolve) => window.requestAnimationFrame(resolve));
  item.classList.add("is-visible");
  await waitForUpdateMessage(message.duration);
  item.classList.remove("is-visible");
  await waitForUpdateMessage(220);
  item.remove();
};

const enqueueUpdateMessage = (text, level = "neutral", duration = 4000) => {
  const normalizedText = String(text || "").trim();
  const normalizedLevel = ["success", "warning", "error", "neutral"].includes(level)
    ? level
    : "neutral";
  if (!normalizedText || !document.querySelector("[data-update-message-queue]")) return;
  const key = `${normalizedLevel}\n${normalizedText}`;
  if (seenUpdateMessages.has(key)) return;
  seenUpdateMessages.add(key);
  void showUpdateMessage({ text: normalizedText, level: normalizedLevel, duration });
};

document.addEventListener("DOMContentLoaded", () => {
  document.querySelectorAll("[data-update-message]").forEach((item) => {
    const level = ["success", "warning", "error", "neutral"].find((value) =>
      item.classList.contains(value),
    );
    enqueueUpdateMessage(item.textContent, level, Number(item.dataset.duration) || 4000);
    item.remove();
  });
  const currentURL = new URL(window.location.href);
  if (currentURL.searchParams.has("update_message") || currentURL.searchParams.has("update_level")) {
    currentURL.searchParams.delete("update_message");
    currentURL.searchParams.delete("update_level");
    window.history.replaceState({}, "", currentURL);
  }
});

// HTMX continues polling while the process restarts. Reload the deployment facts
// once the executor reports a terminal result, including after automatic rollback.
(() => {
  let restarting = false;
  document.addEventListener('htmx:afterSwap', (event) => {
    if (event.detail.target?.id !== 'update-progress') return;
    const node = event.detail.target.querySelector('.update-progress-state');
    if (!node) return;
    if (node.matches('.stopping, .rolling_back, .restarting')) restarting = true;
    if ((restarting || node.dataset.version !== document.body.dataset.appVersion) && node.matches('.success, .failed, .rolled_back')) {
      restarting = false;
      window.location.reload();
      return;
    }
    if (node.matches('.available, .success, .failed, .rolled_back')) {
      let level = 'success';
      if (node.matches('.failed')) level = 'error';
      if (node.matches('.rolled_back')) level = 'warning';
      const duration = level === 'error' ? 6000 : level === 'warning' ? 5000 : 4000;
      enqueueUpdateMessage(node.querySelector('strong')?.textContent, level, duration);
      event.detail.target.replaceChildren();
    }
  });
})();
