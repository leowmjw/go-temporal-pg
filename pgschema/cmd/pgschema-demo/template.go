package main

// The demo UI is a Datastar-driven front end (no build step, no JS
// framework): the server pushes live workflow state over SSE using
// datastar-patch-elements / datastar-patch-signals (Datastar v1.0.2 — see
// server.go's patchElements/patchSignals helpers). There are two pages:
// landingHTML (scenario picker) and scenarioHTML (one scenario's run page).
const datastarScript = `<script type="module" src="https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.2/bundles/datastar.js"></script>`

// commonCSS is shared by both pages (compile-time string concatenation of
// Go string constants — still zero external CSS dependencies).
const commonCSS = `
  :root {
    color-scheme: dark;
    --bg: #0f1117; --panel: #171a23; --border: #2a2f3d;
    --text: #e6e8ee; --muted: #9098ab;
    --accent: #5b8cff; --good: #38c98f; --warn: #e0a83e; --bad: #e0555f;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; padding: 2rem; background: var(--bg); color: var(--text);
    font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
  }
  h1 { font-size: 1.4rem; margin: 0 0 .25rem; }
  .subtitle { color: var(--muted); margin: 0 0 1.5rem; font-size: .9rem; }
  a { color: var(--accent); }
  section.panel {
    background: var(--panel); border: 1px solid var(--border); border-radius: 10px;
    padding: 1.25rem;
  }
  h2 { font-size: 1rem; margin: 0 0 1rem; color: var(--muted); text-transform: uppercase; letter-spacing: .04em; }
  h3 { font-size: .85rem; margin: 0 0 .6rem; color: var(--muted); text-transform: uppercase; letter-spacing: .04em; }
  .muted { color: var(--muted); }
  button {
    background: var(--accent); color: #fff; border: none; border-radius: 6px;
    padding: .5rem .9rem; font-size: .85rem; font-weight: 600; cursor: pointer;
  }
  button:disabled { opacity: .4; cursor: not-allowed; }
  button.danger { background: var(--bad); }
  button.good { background: var(--good); }
  button.reset {
    font-size: 1rem; font-weight: 700; padding: .85rem 1.75rem;
    box-shadow: 0 0 0 3px rgba(224,85,95,.25);
  }
  button.reset:hover:not(:disabled) { background: #ff6b74; }
  .row { display: flex; gap: .5rem; flex-wrap: wrap; margin-top: .5rem; }
  .kv { display: grid; grid-template-columns: auto 1fr; gap: .35rem .75rem; font-size: .9rem; margin-bottom: 1rem; }
  .kv dt { color: var(--muted); }
  .kv dd { margin: 0; font-family: ui-monospace, monospace; }
  #log-lines { font-family: ui-monospace, monospace; font-size: .78rem; max-height: 22rem; overflow-y: auto;
    display: flex; flex-direction: column-reverse; }
  .log-line { padding: .15rem 0; border-bottom: 1px dashed var(--border); }
  .log-line .ts { color: var(--muted); margin-right: .5rem; }
  .badge { display: inline-block; padding: .1rem .5rem; border-radius: 999px; font-size: .75rem; font-weight: 600; }
  .badge.running { background: rgba(91,140,255,.15); color: var(--accent); }
  .badge.completed { background: rgba(56,201,143,.15); color: var(--good); }
  .badge.rolled_back, .badge.rollback_failed, .badge.failed { background: rgba(224,85,95,.15); color: var(--bad); }
  .badge.idle { background: rgba(144,152,171,.15); color: var(--muted); }
`

// ─── Landing page ───────────────────────────────────────────────────────────

const landingHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>pgschema demo — pgroll via Temporal</title>
` + datastarScript + `
<style>` + commonCSS + `
  .scenarios { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 1rem; max-width: 1200px; }
  .scenario { border: 1px solid var(--border); border-radius: 8px; padding: 1rem; display: flex; flex-direction: column; }
  .scenario p { margin: .35rem 0 .75rem; color: var(--muted); font-size: .85rem; line-height: 1.4; flex: 1; }
</style>
</head>
<body data-signals="{resetting:false}">

<h1>pgschema demo</h1>
<p class="subtitle">Zero-downtime pgroll schema migrations, orchestrated live by a Temporal workflow. Pick a scenario to open its run page.</p>

<div style="margin-bottom:1.5rem">
  <button class="danger reset"
    data-on:click="confirm('Reset the demo database? This wipes all progress — scenarios will need to be re-run from #1.') && @post('/reset')"
    data-attr:disabled="$resetting"
    data-text="$resetting ? '⏳ Resetting…' : '🔄 Demo stuck? Reset database'"></button>
</div>

<div class="scenarios">
  {{range .Scenarios}}
  <a class="scenario" href="/scenario/{{.ID}}" style="text-decoration:none;color:inherit;background:var(--panel);border-color:var(--border)">
    <strong>{{.Title}}</strong>
    <p>{{.Description}}</p>
    <span style="color:var(--accent);font-weight:600;font-size:.85rem">Open →</span>
  </a>
  {{end}}
</div>

</body>
</html>
`

// ─── Scenario page ──────────────────────────────────────────────────────────

const scenarioHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Scenario.Title}} — pgschema demo</title>
` + datastarScript + `
<style>` + commonCSS + `
  main { max-width: 1200px; }
  .bottom-row { display: grid; grid-template-columns: 1fr 1fr; gap: 1.5rem; margin-top: 1.5rem; }
  @media (max-width: 900px) { .bottom-row { grid-template-columns: 1fr; } }

  .plan-tabs { display: flex; gap: .5rem; margin-bottom: 1rem; }
  .plan-tabs button { background: transparent; border: 1px solid var(--border); color: var(--muted); }
  .plan-tabs button.active { background: var(--accent); color: #fff; border-color: var(--accent); }

  .diff-line { font-family: ui-monospace, monospace; font-size: .85rem; padding: .3rem .6rem; border-radius: 4px; margin-bottom: .3rem; }
  .diff-line.add { background: rgba(56,201,143,.12); color: var(--good); }
  .diff-line.del { background: rgba(224,85,95,.12); color: var(--bad); text-decoration: line-through; }
  .diff-line.neutral { background: rgba(144,152,171,.08); color: var(--muted); }

  .plan-graph { display: flex; align-items: center; flex-wrap: wrap; gap: .5rem; }
  .graph-node { border: 1px solid var(--border); border-radius: 8px; padding: .5rem .75rem; display: flex; flex-direction: column; font-size: .8rem; min-width: 8rem; }
  .graph-node strong { font-size: .85rem; }
  .graph-node span { color: var(--muted); font-family: ui-monospace, monospace; }
  .graph-node.active { border-color: var(--accent); box-shadow: 0 0 0 2px rgba(91,140,255,.25); }
  .graph-node.done { border-color: var(--good); background: rgba(56,201,143,.08); }
  .graph-arrow { color: var(--muted); }

  .version-panel { margin-top: 1.25rem; padding-top: 1.25rem; border-top: 1px dashed var(--border); }
  .version-row { display: flex; gap: 1rem; flex-wrap: wrap; }
  .version-card { border: 1px solid var(--border); border-radius: 8px; padding: .75rem 1rem; flex: 1; min-width: 14rem; }
  .version-card.active { border-color: var(--accent); box-shadow: 0 0 0 2px rgba(91,140,255,.25); }
  .version-card.cleaned { opacity: .35; pointer-events: none; }
  .version-card-head { display: flex; align-items: center; gap: .5rem; margin-bottom: .5rem; }
  .version-cols { list-style: none; margin: 0 0 .75rem; padding: 0; font-size: .8rem; }
  .version-cols li { padding: .1rem 0; }
  .backfill-status { margin-top: 1rem; font-size: .85rem; }

  .bar-track { width: 100%; height: .6rem; border-radius: 999px; background: var(--border); overflow: hidden; }
  .bar-fill { height: 100%; background: var(--accent); transition: width .5s ease; border-radius: 999px; }
  .steps { display: flex; gap: .35rem; margin: .5rem 0 1rem; flex-wrap: wrap; }
  .step-dot { font-size: .7rem; padding: .15rem .5rem; border-radius: 999px; border: 1px solid var(--border); color: var(--muted); }
  .step-dot.active { border-color: var(--accent); color: var(--accent); }
  .step-dot.done { border-color: var(--good); color: var(--good); background: rgba(56,201,143,.08); }
  .spinner { display: inline-block; width: .8rem; height: .8rem; border: 2px solid var(--border); border-top-color: var(--accent);
    border-radius: 50%; animation: spin .7s linear infinite; margin-right: .4rem; vertical-align: -1px; }
  @keyframes spin { to { transform: rotate(360deg); } }
</style>
</head>
<body data-signals="{phase:'idle',status:'idle',percent:0,message:'',workflowId:'',runId:''}"
      data-init="@get('/status')">

<p><a href="/">← all scenarios</a>{{if .Next}} · <a href="/scenario/{{.Next.ID}}">next: {{.Next.Title}} →</a>{{end}}</p>
<h1>{{.Scenario.Title}}</h1>
<p class="subtitle">{{.Scenario.Description}}</p>
<div class="row" style="margin-bottom:1.5rem">
  <button data-on:click="@post('/scenario/{{.Scenario.ID}}/start')" data-attr:disabled="$status=='running'">▶ Run this scenario</button>
</div>

<main>
  <section class="panel" id="plan-panel">
    <h2>Migration plan</h2>
    <div class="plan-tabs" data-signals="{planView:'diff'}">
      <button data-class="{active:$planView=='diff'}" data-on:click="$planView='diff'">Diff</button>
      <button data-class="{active:$planView=='graph'}" data-on:click="$planView='graph'">Graph</button>
    </div>
    <div data-show="$planView=='diff'">{{.PlanDiff}}</div>
    <div data-show="$planView=='graph'">{{.PlanGraph}}</div>

    <div data-show="$percent >= 5"><div id="version-panel" class="version-panel"><p class="muted">no versioned schemas yet — waiting for expand to start</p></div></div>
  </section>

  <div class="bottom-row">
    <section class="panel">
      <h2>Activity log</h2>
      <div id="log-lines"></div>
    </section>

    <section class="panel" id="workflow-state">
      <h2>Live workflow state</h2>
      <dl class="kv">
        <dt>Workflow</dt><dd data-text="$workflowId || '—'"></dd>
        <dt>Status</dt><dd><span class="badge" data-class="{running:$status=='running',completed:$status=='completed',rolled_back:$status=='rolled_back',rollback_failed:$status=='rollback_failed',idle:$status=='idle'}" data-text="$status"></span></dd>
        <dt>Phase</dt><dd><span class="spinner" data-show="$phase=='preflighting'||$phase=='validating'"></span><span data-text="$phase"></span></dd>
      </dl>

      <div class="steps">
        <span class="step-dot" data-class="{done:$percent>1,active:$percent>=1&&$percent<5}">preflight</span>
        <span class="step-dot" data-class="{done:$percent>5,active:$percent>=5&&$percent<20}">validate</span>
        <span class="step-dot" data-class="{done:$percent>20,active:$percent>=20&&$percent<40}">start (expand)</span>
        <span class="step-dot" data-class="{done:$percent>40,active:$percent>=40&&$percent<70}">wait for app-ready</span>
        <span class="step-dot" data-class="{done:$percent>70,active:$percent>=70&&$percent<90}">complete (contract)</span>
        <span class="step-dot" data-class="{done:$percent>90,active:$percent>=90&&$percent<100}">verify</span>
        <span class="step-dot" data-class="{done:$percent>=100}">done</span>
      </div>
      <div class="bar-track"><div class="bar-fill" data-attr:style="'width:' + $percent + '%'"></div></div>
      <p style="text-align:right;margin:.25rem 0 0"><span data-text="$percent"></span>%</p>

      <div data-show="$message != ''" style="margin:1rem 0;color:var(--warn);font-size:.85rem" data-text="$message"></div>

      <div class="row">
        <button class="good" data-on:click="@post('/signal/app-ready')" data-show="$phase=='waiting_for_app_ready'">✅ Send app-ready (deploy on new schema)</button
        ><button class="danger" data-on:click="@post('/signal/rollback')" data-show="$status=='running'">⛔ Abort / rollback</button>
      </div>
    </section>
  </div>
</main>

</body>
</html>
`
