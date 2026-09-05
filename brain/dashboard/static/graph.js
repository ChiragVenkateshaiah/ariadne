// Renders the World Model topology with cytoscape.js. Node color is
// categorical by kind, in a fixed order (dataviz skill's rule: assign
// categorical hues in fixed order, never cycled) -- the same kind always
// gets the same color across reloads and across the legend.
const KIND_COLOR = {
  SERVICE: "#3987e5",
  WORKLOAD: "#d95926",
  WORKFLOW: "#9085e9",
  WORKFLOW_STEP: "#d55181",
  CONFIG_RESOURCE: "#199e70",
  SECRET: "#e66767",
  SERVICE_ACCOUNT: "#c98500",
  ROLE: "#008300",
  ROLE_BINDING: "#008300",
  NETWORK_POLICY: "#e66767",
  API_ENDPOINT: "#3987e5",
  UI_ROUTE: "#3987e5",
  INGRESS: "#c98500",
};
const DEFAULT_COLOR = "#8a8a80";

function kindColor(kind) {
  return KIND_COLOR[kind] || DEFAULT_COLOR;
}

async function loadGraph() {
  const res = await fetch("/api/graph");
  const data = await res.json();

  const elements = [
    ...data.nodes.map((n) => ({
      data: { id: n.id, label: n.name, kind: n.kind },
    })),
    ...data.edges
      .filter((e) => data.nodes.some((n) => n.id === e.src_id) && data.nodes.some((n) => n.id === e.dst_id))
      .map((e) => ({
        data: { id: e.id, source: e.src_id, target: e.dst_id, kind: e.kind },
      })),
  ];

  const cy = cytoscape({
    container: document.getElementById("cy"),
    elements,
    style: [
      {
        selector: "node",
        style: {
          "background-color": (n) => kindColor(n.data("kind")),
          label: "data(label)",
          "font-size": 9,
          color: "#c3c2b7",
          "text-valign": "bottom",
          "text-margin-y": 4,
          width: 22,
          height: 22,
          "border-width": 2,
          "border-color": "#1a1a19",
        },
      },
      {
        selector: "edge",
        style: {
          width: 1.2,
          "line-color": "#38382f",
          "target-arrow-color": "#38382f",
          "target-arrow-shape": "triangle",
          "arrow-scale": 0.7,
          "curve-style": "bezier",
          opacity: 0.6,
        },
      },
    ],
    layout: { name: "cose", animate: false, padding: 20 },
  });

  const legend = document.getElementById("graph-legend");
  const seenKinds = [...new Set(data.nodes.map((n) => n.kind))];
  legend.innerHTML = seenKinds
    .map((k) => `<span><span class="legend-dot" style="background:${kindColor(k)}"></span>${k}</span>`)
    .join("");

  return cy;
}

let _cy = null;
loadGraph().then((cy) => (_cy = cy));
// Refresh the topology periodically -- cheap enough at demo scale, and
// means a newly-discovered node (e.g. a new workflow synthesized live)
// appears without a manual page reload.
setInterval(() => {
  if (_cy) _cy.destroy();
  loadGraph().then((cy) => (_cy = cy));
}, 8000);
