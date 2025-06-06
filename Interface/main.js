// main.js

// Keep an in‐memory map from agentID → <tr> element:
const agentRows = {};

// Utility: Given an ISO‐8601 timestamp string, produce a localized Date/Time string.
function formatTime(iso) {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

// Updates the “Online Agents” counter by counting rows with class “online”
function updateOnlineCount() {
  const count = document.querySelectorAll("tbody tr.online").length;
  document.getElementById("onlineCount").textContent = count;

  // Show the blinking dot only if there's at least one online agent
  const dot = document.getElementById("blinkDot");
  if (count > 0) {
    dot.style.visibility = "visible";
  } else {
    dot.style.visibility = "hidden";
  }
}

// Called whenever we receive an "agent-status-changed" event:
function onAgentStatusChanged(payload) {
  // payload: { agentID: "1.2.3.4", status: { Online: bool, Status: string, LastPing: string } }
  const agentID = payload.agentID;
  const info = payload.status;

  let row = agentRows[agentID];
  if (!row) {
    // Create a new <tr> with four <td> cells: Agent, Online, Status, LastPing
    row = document.createElement("tr");
    row.setAttribute("data-agentid", agentID);

    // 1) Agent cell
    const tdAgent = document.createElement("td");
    tdAgent.textContent = agentID;
    row.appendChild(tdAgent);

    // 2) Online cell
    const tdOnline = document.createElement("td");
    tdOnline.classList.add("online-cell");
    row.appendChild(tdOnline);

    // 3) Status cell
    const tdStatus = document.createElement("td");
    tdStatus.classList.add("status-cell");
    row.appendChild(tdStatus);

    // 4) Last Ping cell
    const tdLastPing = document.createElement("td");
    tdLastPing.classList.add("lastping-cell");
    row.appendChild(tdLastPing);

    document.querySelector("tbody").appendChild(row);
    agentRows[agentID] = row;
  }

  // Update “Online” cell (Online vs Offline) and row class
  const tdOnline = row.querySelector(".online-cell");
  if (info.Online) {
    tdOnline.textContent = "Online";
    row.classList.remove("offline");
    row.classList.add("online");
  } else {
    tdOnline.textContent = "Offline";
    row.classList.remove("online");
    row.classList.add("offline");
  }

  // Update Status cell
  const tdStatus = row.querySelector(".status-cell");
  tdStatus.textContent = info.Status || "";

  // Update LastPing cell
  const tdLastPing = row.querySelector(".lastping-cell");
  tdLastPing.textContent = info.LastPing ? formatTime(info.LastPing) : "";

  // Recalculate how many agents are online
  updateOnlineCount();
}

// Called whenever we receive a "command-enqueued" event.
// (Optional: highlight agent rows, etc.)
function onCommandEnqueued(payload) {
  // payload: { agentID: "1.2.3.4", command: { Action, URL, Threads, Timer, CustomHost } }
  console.log("Command enqueued for agent:", payload.agentID, payload.command);
  // You could add visual feedback here (e.g., a small icon or animation on that agent’s row)
}

document.addEventListener("DOMContentLoaded", () => {
  // 1) Fetch initial snapshot of /agent-statuses
  fetch("/agent-statuses")
    .then((res) => res.json())
    .then((allStatuses) => {
      // allStatuses is an object: { "1.2.3.4": { Online, Status, LastPing }, ... }
      for (const [agentID, info] of Object.entries(allStatuses)) {
        onAgentStatusChanged({ agentID: agentID, status: info });
      }
      // Make sure counter is correct right after the initial load
      updateOnlineCount();
    })
    .catch((err) => {
      console.warn("Could not fetch initial agent-statuses:", err);
    });

  // 2) Open a long‐lived SSE connection to /events (no ?agentID)
  const evtSource = new EventSource("/events");

  evtSource.addEventListener("agent-status-changed", (ev) => {
    const payload = JSON.parse(ev.data);
    onAgentStatusChanged(payload);
  });

  evtSource.addEventListener("command-enqueued", (ev) => {
    const payload = JSON.parse(ev.data);
    onCommandEnqueued(payload);
  });

  evtSource.onerror = (err) => {
    console.error("SSE error:", err);
    // Optionally: display a banner or retry logic
  };
});
