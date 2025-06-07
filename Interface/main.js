// main.js

document.addEventListener("DOMContentLoaded", () => {
  const agentBody   = document.querySelector(".form-container .scroller table tbody");
  const historyBody = document.getElementById("historyBody");
  const blinkDot    = document.getElementById("blinkDot");
  const onlineCount = document.getElementById("onlineCount");
  const form        = document.getElementById("commandForm");
  const agentRows = {};

  function formatTime(iso) {
    try { return new Date(iso).toLocaleString(); }
    catch { return iso; }
  }

  function updateOnlineCount() {
    const count = agentBody.querySelectorAll("tr.online").length;
    onlineCount.textContent = count;
    blinkDot.style.visibility = count > 0 ? "visible" : "hidden";
  }

  function onAgentStatusChanged({ agentID, status: info }) {
    let row = agentRows[agentID];
    if (!row) {
      row = document.createElement("tr");
      row.setAttribute("data-agentid", agentID);
      const tdAgent = document.createElement("td"); tdAgent.textContent = agentID;
      const tdOnline = document.createElement("td"); tdOnline.classList.add("online-cell");
      const tdStatus = document.createElement("td"); tdStatus.classList.add("status-cell");
      const tdLastPing = document.createElement("td"); tdLastPing.classList.add("lastping-cell");
      row.append(tdAgent, tdOnline, tdStatus, tdLastPing);
      agentBody.appendChild(row);
      agentRows[agentID] = row;
    }
    const tdOnline = row.querySelector(".online-cell");
    if (info.Online) {
      tdOnline.textContent = "Online";
      row.classList.add("online");
      row.classList.remove("offline");
    } else {
      tdOnline.textContent = "Offline";
      row.classList.add("offline");
      row.classList.remove("online");
    }
    row.querySelector(".status-cell").textContent = info.Status || "";
    row.querySelector(".lastping-cell").textContent =
      info.LastPing ? formatTime(info.LastPing) : "";
    updateOnlineCount();
  }

  function addHistoryRow(command) {
    const tr  = document.createElement("tr");
    const now = new Date().toLocaleString();
    const tdTime = document.createElement("td");
    tdTime.textContent = now;
    tr.appendChild(tdTime);
    ["url", "threads", "timer", "custom_host"].forEach(key => {
      const td = document.createElement("td");
      td.textContent = command[key] || "";
      tr.appendChild(td);
    });
    historyBody.prepend(tr);
  }

  function onCommandEnqueued({ command }) {
    if (command) addHistoryRow(command);
  }

  form.addEventListener("submit", async e => {
    e.preventDefault();
    const formData = new FormData(form);
    const body = new URLSearchParams(formData);

    try {
      console.log("Submitting command:", body.toString());
      const res = await fetch("/command", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: body.toString()
      });
      if (!res.ok) throw new Error(`Enqueue failed: ${res.status}`);
      // Reflect back raw values
      const cmd = Object.fromEntries(formData.entries());
      addHistoryRow(cmd);
      // Clear form inputs
      form.reset();
    } catch (err) {
      console.error(err);
      alert(err.message);
    }
  });

  fetch("/agent-statuses")
    .then(r => r.json())
    .then(all => {
      Object.entries(all).forEach(([id, info]) =>
        onAgentStatusChanged({ agentID: id, status: info })
      );
    })
    .catch(console.warn);

  const evt = new EventSource("/events");
  evt.addEventListener("agent-status-changed", ev =>
    onAgentStatusChanged(JSON.parse(ev.data))
  );
  evt.addEventListener("command-enqueued", ev =>
    onCommandEnqueued(JSON.parse(ev.data))
  );
  evt.onerror = err => console.error("SSE error:", err);
});
