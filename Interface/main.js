// main.js

document.addEventListener("DOMContentLoaded", () => {
  const agentBody      = document.getElementById("agentBody");
  const agentEmptyRow  = document.getElementById("agentEmptyRow");
  const historyBody    = document.getElementById("historyBody");
  const historyEmpty   = document.getElementById("historyEmptyRow");
  const agentCounter   = document.getElementById("agentCounter");
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

  /** Escape HTML special characters to prevent XSS when inserting into innerHTML. */
  function escapeHTML(str) {
    if (str == null) return "";
    return String(str)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
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

  // ── Form interactions ──────────────────────────────────────────────────────
  
  const methodSelect = document.getElementById("method");
  const proxyTypeField = document.getElementById("proxyTypeField");
  
  if (methodSelect && proxyTypeField) {
    methodSelect.addEventListener("change", () => {
      if (methodSelect.value === "l7p") {
        proxyTypeField.style.display = "";
      } else {
        proxyTypeField.style.display = "none";
      }
    });
  }

  // ── Agent status ───────────────────────────────────────────────────────────

  function updateOnlineCount() {
    const count = agentBody.querySelectorAll("tr.online").length;
    onlineCount.textContent = count;
    agentCounter.classList.toggle("has-agents", count > 0);
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
      row.classList.add("history-new"); // reuse history animation class
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
      setTimeout(() => row.classList.add("visible"), 10);
    }

    const idCell = row.querySelector(".agent-id-cell");
    idCell.textContent = agentID;
    idCell.title = agentID;

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
 
  // command: { action, url, threads, timer, custom_host, method, proxy_type }
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
 
    const fields = ["method", "proxy_type", "url", "threads", "timer", "custom_host"];
    tr.innerHTML = `<td>${escapeHTML(new Date().toLocaleString())}</td>` +
      fields.map(k => {
        let val = command[k];
        if (k === "method") {
          val = val === "l7" ? "Direct" : (val === "l7p" ? "Proxy" : val);
        }
        return `<td>${val ? escapeHTML(String(val)) : "—"}</td>`;
      }).join("");
 
    historyBody.insertBefore(tr, historyBody.firstChild);
    setTimeout(() => tr.classList.add("visible"), 10);
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
    if (formData.get("method") !== "l7p") {
      formData.delete("proxy_type");
    }

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
        redirect: "manual",
        headers: {
          "Content-Type": "application/x-www-form-urlencoded",
          ...authHeaders(),
        },
        body: new URLSearchParams(formData).toString(),
      });

      // redirect: 'manual' returns an opaque redirect (status 0) — treat as success.
      if (!res.ok && res.type !== "opaqueredirect" && res.status !== 0) {
        const text = await res.text();
        throw new Error(text || `Server returned ${res.status}`);
      }

      // The SSE "command-enqueued" event will fire and add the history row.
      // We do NOT add it here too — that would cause duplicate rows.
      form.reset();
      if (methodSelect) {
        methodSelect.dispatchEvent(new Event("change"));
      }
    } catch (err) {
      showFormError(err.message);
    } finally {
      setSubmitting(false);
    }
  });

  const stopBtn = document.getElementById("stopBtn");
  if (stopBtn) {
    stopBtn.addEventListener("click", async () => {
      clearFormError();
      setSubmitting(true);
      try {
        const res = await fetch("/command", {
          method: "POST",
          headers: {
            "Content-Type": "application/x-www-form-urlencoded",
            ...authHeaders(),
          },
          body: new URLSearchParams({ action: "stop" }).toString(),
        });

        if (!res.ok) {
          const text = await res.text();
          throw new Error(text || `Server returned ${res.status}`);
        }
      } catch (err) {
        showFormError(err.message);
      } finally {
        setSubmitting(false);
      }
    });
  }

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
          const fields = ["method", "proxy_type", "url", "threads", "timer", "custom_host"];
          tr.innerHTML = `<td>${escapeHTML(new Date().toLocaleString())}</td>` +
            fields.map(k => {
              let val = cmd[k];
              if (k === "method") {
                val = val === "l7" ? "Direct" : (val === "l7p" ? "Proxy" : val);
              }
              return `<td>${val != null ? escapeHTML(String(val)) : "—"}</td>`;
            }).join("");
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

  // ── Proxy Scraper ──────────────────────────────────────────────────────────

  const scrapeProxiesBtn   = document.getElementById("scrapeProxiesBtn");
  const sendProxiesBtn     = document.getElementById("sendProxiesBtn");
  const modalScrapeBtn     = document.getElementById("modalScrapeBtn");
  const proxyModal         = document.getElementById("proxyModal");
  const proxyModalClose    = document.getElementById("proxyModalClose");
  const proxyProtocol      = document.getElementById("proxyProtocol");
  const proxyList          = document.getElementById("proxyList");
  const proxyPlaceholder   = document.getElementById("proxyPlaceholder");
  const proxyCountBadge    = document.getElementById("proxyCountBadge");
  const proxyTestProgress  = document.getElementById("proxyTestProgress");
  const proxyTestProgressFill = document.getElementById("proxyTestProgressFill");
  const copyProxiesBtn     = document.getElementById("copyProxiesBtn");
  const downloadProxiesBtn = document.getElementById("downloadProxiesBtn");
  const clearProxiesBtn    = document.getElementById("clearProxiesBtn");
  const testProxiesBtn     = document.getElementById("testProxiesBtn");

  let scrapedProxies = [];

  // ── Helpers ────────────────────────────────────────────────────────────────

  function openProxyModal() {
    proxyModal.hidden = false;
    document.body.style.overflow = "hidden";
  }

  function closeProxyModal() {
    proxyModal.hidden = true;
    document.body.style.overflow = "";
  }

  function setProxyActionBtns(enabled) {
    copyProxiesBtn.disabled     = !enabled;
    downloadProxiesBtn.disabled = !enabled;
    clearProxiesBtn.disabled    = !enabled;
    testProxiesBtn.disabled     = !enabled;
    sendProxiesBtn.disabled     = !enabled;
  }

  function getProxyStrings() {
    return scrapedProxies.map(p => typeof p === 'object' ? p.proxy : p);
  }

  function renderProxies(proxies) {
    proxyPlaceholder.style.display = "none";
    proxyList.innerHTML = proxies
      .map(p => {
        const isObj = typeof p === 'object';
        const proxyStr = isObj ? p.proxy : p;
        const safeStr = escapeHTML(proxyStr);
        const latStr = isObj ? `<span style="float:right; opacity:0.6; font-size:0.75rem;">${escapeHTML(p.latency)}ms</span>` : '';
        return `<div class="proxy-entry" title="Click to copy" data-proxy="${safeStr}">${safeStr}${latStr}</div>`;
      })
      .join("");

    proxyList.querySelectorAll(".proxy-entry").forEach(el => {
      el.addEventListener("click", () => {
        const proxyStr = el.getAttribute("data-proxy");
        navigator.clipboard.writeText(proxyStr).then(() => {
          const orig = el.innerHTML;
          el.innerHTML = "✓ Copied";
          setTimeout(() => { el.innerHTML = orig; }, 1200);
        });
      });
    });
  }

  // ── Scrape ─────────────────────────────────────────────────────────────────

  async function doScrape() {
    const protocol = proxyProtocol.value;
    proxyList.innerHTML = "";
    proxyPlaceholder.style.display = "";
    proxyPlaceholder.innerHTML = `<span class="proxy-loading">Scraping ${protocol.toUpperCase()} proxies from sources…</span>`;
    proxyCountBadge.textContent = "Scraping…";
    proxyTestProgress.hidden = true;
    setProxyActionBtns(false);
    scrapeProxiesBtn.disabled = true;
    modalScrapeBtn.disabled   = true;

    try {
      const res = await fetch(`/scrape-proxies?protocol=${protocol}`);
      if (!res.ok) throw new Error(`Server returned ${res.status}`);
      const data = await res.json();
      scrapedProxies = data.proxies || [];
      proxyCountBadge.textContent = `${scrapedProxies.length.toLocaleString()} ${protocol.toUpperCase()} proxies`;
      if (scrapedProxies.length === 0) {
        proxyPlaceholder.style.display = "";
        proxyPlaceholder.textContent = "No proxies returned. Sources may be unreachable.";
      } else {
        renderProxies(scrapedProxies);
        setProxyActionBtns(true);
      }
    } catch (err) {
      proxyPlaceholder.style.display = "";
      proxyPlaceholder.innerHTML = `<span class="proxy-error">Error: ${err.message}</span>`;
      proxyCountBadge.textContent = "Failed";
    } finally {
      scrapeProxiesBtn.disabled = false;
      modalScrapeBtn.disabled   = false;
    }
  }

  // ── Clear ──────────────────────────────────────────────────────────────────

  function doClear() {
    scrapedProxies = [];
    proxyList.innerHTML = "";
    proxyPlaceholder.style.display = "";
    proxyPlaceholder.textContent = "Select a protocol and click ↻ Scrape.";
    proxyCountBadge.textContent = "—";
    proxyTestProgress.hidden = true;
    setProxyActionBtns(false);
  }

  // ── Test Proxies ───────────────────────────────────────────────────────────

  async function doTest() {
    if (scrapedProxies.length === 0) return;

    const toTest = getProxyStrings();
    const timeoutMs = parseInt(document.getElementById("testTimeoutMs").value) || 3000;
    
    setProxyActionBtns(false);
    modalScrapeBtn.disabled = true;
    
    proxyTestProgress.hidden = false;
    proxyTestProgressFill.style.width = "0%";
    proxyCountBadge.textContent = `Testing ${toTest.length} proxies… 0%`;

    let allWorking = [];
    const batchSize = 250;

    try {
      for (let i = 0; i < toTest.length; i += batchSize) {
        const batch = toTest.slice(i, i + batchSize);
        const res = await fetch("/test-proxies", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...authHeaders() },
          body: JSON.stringify({ proxies: batch, timeout_ms: timeoutMs }),
        });
        
        if (!res.ok) throw new Error(`Server returned ${res.status}`);
        const data = await res.json();
        
        if (data.working) {
          allWorking = allWorking.concat(data.working);
        }
        
        const progressPercent = Math.min(100, Math.round(((i + batch.length) / toTest.length) * 100));
        proxyTestProgressFill.style.width = `${progressPercent}%`;
        proxyCountBadge.textContent = `Testing ${toTest.length} proxies… ${progressPercent}%`;
      }

      scrapedProxies = allWorking;
      const removed = toTest.length - allWorking.length;
      proxyCountBadge.textContent =
        `${allWorking.length.toLocaleString()} working  (${removed} removed)`;
      proxyTestProgress.hidden = true;

      if (allWorking.length === 0) {
        proxyList.innerHTML = "";
        proxyPlaceholder.style.display = "";
        proxyPlaceholder.textContent = "No working proxies found.";
        setProxyActionBtns(false);
      } else {
        renderProxies(allWorking);
        setProxyActionBtns(true);
      }
    } catch (err) {
      proxyTestProgress.hidden = true;
      proxyCountBadge.textContent = "Test failed";
      setProxyActionBtns(true);
    } finally {
      modalScrapeBtn.disabled = false;
    }
  }

  // ── Wire events ────────────────────────────────────────────────────────────

  scrapeProxiesBtn.addEventListener("click", openProxyModal);
  modalScrapeBtn.addEventListener("click", doScrape);
  clearProxiesBtn.addEventListener("click", doClear);
  testProxiesBtn.addEventListener("click", doTest);
  proxyModalClose.addEventListener("click", closeProxyModal);

  sendProxiesBtn.addEventListener("click", async () => {
    if (scrapedProxies.length === 0) return;
    const toSend = getProxyStrings();
    const orig = sendProxiesBtn.textContent;
    sendProxiesBtn.disabled = true;
    sendProxiesBtn.textContent = "Sending…";
    try {
      const res = await fetch("/broadcast-proxies", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: JSON.stringify({ proxies: toSend }),
      });
      if (!res.ok) throw new Error("Failed");
      sendProxiesBtn.textContent = "✓ Sent";
    } catch (err) {
      sendProxiesBtn.textContent = "✗ Error";
    } finally {
      setTimeout(() => {
        sendProxiesBtn.textContent = orig;
        sendProxiesBtn.disabled = false;
      }, 2000);
    }
  });

  proxyModal.addEventListener("click", e => {
    if (e.target === proxyModal) closeProxyModal();
  });

  document.addEventListener("keydown", e => {
    if (e.key === "Escape" && !proxyModal.hidden) closeProxyModal();
  });

  copyProxiesBtn.addEventListener("click", () => {
    navigator.clipboard.writeText(getProxyStrings().join("\n")).then(() => {
      const orig = copyProxiesBtn.textContent;
      copyProxiesBtn.textContent = "✓ Copied!";
      setTimeout(() => { copyProxiesBtn.textContent = orig; }, 1500);
    });
  });

  downloadProxiesBtn.addEventListener("click", () => {
    const blob = new Blob([getProxyStrings().join("\n")], { type: "text/plain" });
    const url  = URL.createObjectURL(blob);
    const a    = document.createElement("a");
    a.href     = url;
    a.download = `proxies_${proxyProtocol.value}_${new Date().toISOString().slice(0,10)}.txt`;
    a.click();
    URL.revokeObjectURL(url);
  });

});