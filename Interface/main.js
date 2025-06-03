async function fetchAgentStatuses() {
  const response = await fetch('/agent-statuses');
  const data = await response.json();

  const tbody = document.querySelector('tbody');
  tbody.innerHTML = '';

  for (const [agent, info] of Object.entries(data)) {
    const row = '<tr>' +
      '<td>' + agent + '</td>' +
      '<td>' + (info.Online ? 'Online' : 'Offline') + '</td>' +
      '<td>' + info.Status + '</td>' +
      '<td>' + new Date(info.LastPing).toLocaleString() + '</td>' +
      '</tr>';
    tbody.innerHTML += row;
  }
}

setInterval(fetchAgentStatuses, 5000);
fetchAgentStatuses();
