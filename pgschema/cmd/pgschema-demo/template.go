package main

// indexHTML is the demo page: a Datastar-driven front end (no build step, no
// JS framework) that starts scenarios and reflects live workflow state
// pushed from the server over SSE. Datastar's current stable release is
// v1.0.2 (there is no published v2 as of this writing); the wire protocol
// it speaks — datastar-patch-elements / datastar-patch-signals — is what
// server.go's patchElements/patchSignals helpers implement.
const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>pgschema demo — pgroll via Temporal</title>
<script type="module" src="https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.2/bundles/datastar.js"></script>
<style>
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
  main { display: grid; grid-template-columns: 1.1fr .9fr; gap: 1.5rem; max-width: 1200px; }
  @media (max-width: 900px) { main { grid-template-columns: 1fr; } }
  section.panel {
    background: var(--panel); border: 1px solid var(--border); border-radius: 10px;
    padding: 1.25rem;
  }
  h2 { font-size: 1rem; margin: 0 0 1rem; color: var(--muted); text-transform: uppercase; letter-spacing: .04em; }
  .scenario { border: 1px solid var(--border); border-radius: 8px; padding: .75rem 1rem; margin-bottom: .75rem; }
  .scenario:last-child { margin-bottom: 0; }
  .scenario p { margin: .35rem 0 .6rem; color: var(--muted); font-size: .85rem; line-height: 1.4; }
  button {
    background: var(--accent); color: #fff; border: none; border-radius: 6px;
    padding: .5rem .9rem; font-size: .85rem; font-weight: 600; cursor: pointer;
  }
  button:disabled { opacity: .4; cursor: not-allowed; }
  button.danger { background: var(--bad); }
  button.good { background: var(--good); }
  .row { display: flex; gap: .5rem; flex-wrap: wrap; margin-top: .5rem; }
  .kv { display: grid; grid-template-columns: auto 1fr; gap: .35rem .75rem; font-size: .9rem; margin-bottom: 1rem; }
  .kv dt { color: var(--muted); }
  .kv dd { margin: 0; font-family: ui-monospace, monospace; }
  progress { width: 100%; height: .6rem; accent-color: var(--accent); }
  #log-lines { font-family: ui-monospace, monospace; font-size: .78rem; max-height: 22rem; overflow-y: auto;
    display: flex; flex-direction: column-reverse; }
  .log-line { padding: .15rem 0; border-bottom: 1px dashed var(--border); }
  .log-line .ts { color: var(--muted); margin-right: .5rem; }
  .badge { display: inline-block; padding: .1rem .5rem; border-radius: 999px; font-size: .75rem; font-weight: 600; }
  .badge.running { background: rgba(91,140,255,.15); color: var(--accent); }
  .badge.completed { background: rgba(56,201,143,.15); color: var(--good); }
  .badge.rolled_back, .badge.rollback_failed, .badge.failed { background: rgba(224,85,95,.15); color: var(--bad); }
  .badge.idle { background: rgba(144,152,171,.15); color: var(--muted); }
</style>
</head>
<body data-signals="{phase:'idle',status:'idle',percent:0,message:'',workflowId:'',runId:''}"
      data-init="@get('/status')">

<h1>pgschema demo</h1>
<p class="subtitle">Zero-downtime pgroll schema migrations, orchestrated live by a Temporal workflow. Click a scenario to run it against the demo database.</p>

<main>
  <section class="panel" id="scenarios">
    <h2>Scenarios (basic → complex)</h2>
    {{range .Scenarios}}
    <div class="scenario">
      <strong>{{.Title}}</strong>
      <p>{{.Description}}</p>
      <button data-on:click="@post('/scenario/{{.ID}}/start')" data-attr:disabled="$status=='running'">Run</button>
    </div>
    {{end}}
  </section>

  <section class="panel">
    <h2>Live workflow state</h2>
    <dl class="kv">
      <dt>Workflow</dt><dd data-text="$workflowId || '—'"></dd>
      <dt>Status</dt><dd><span class="badge" data-class="{running:$status=='running',completed:$status=='completed',rolled_back:$status=='rolled_back',rollback_failed:$status=='rollback_failed',idle:$status=='idle'}" data-text="$status"></span></dd>
      <dt>Phase</dt><dd data-text="$phase"></dd>
      <dt>Progress</dt><dd><progress data-attr:value="$percent" max="100"></progress> <span data-text="$percent + '%'"></span></dd>
    </dl>
    <div data-show="$message != ''" style="margin-bottom:1rem;color:var(--warn);font-size:.85rem" data-text="$message"></div>

    <div class="row">
      <button class="good" data-on:click="@post('/signal/app-ready')" data-show="$phase=='waiting_for_app_ready'">✅ Send app-ready</button>
      <button class="danger" data-on:click="@post('/signal/rollback')" data-show="$status=='running'">⛔ Abort / rollback</button>
    </div>
  </section>
</main>

<section class="panel" style="max-width:1200px;margin-top:1.5rem">
  <h2>Activity log</h2>
  <div id="log-lines"></div>
</section>

</body>
</html>
`
