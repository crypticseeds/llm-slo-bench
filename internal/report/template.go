package report

const htmlTemplate = `{{define "header"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark light">
<title>llm-slo-bench report</title>
<style>
:root{color-scheme:dark;--bg:#0b0d10;--panel:#14181d;--ink:#f2f0e9;--muted:#9da6b0;--line:#303843;--accent:#55d6be;--pending:#f4b942;--bad:#ff7a90}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace}main{max-width:1180px;margin:auto;padding:40px 24px 80px}header{border-bottom:1px solid var(--line);padding-bottom:24px}h1{font-size:clamp(30px,6vw,58px);line-height:1;margin:10px 0 14px;letter-spacing:-.04em}h2{font-size:15px;letter-spacing:.08em;text-transform:uppercase;margin:42px 0 14px}.eyebrow,.muted{color:var(--muted)}.eyebrow{letter-spacing:.12em;text-transform:uppercase}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:1px;background:var(--line);border:1px solid var(--line)}.cell{background:var(--panel);padding:15px;min-width:0}.label{display:block;color:var(--muted);font-size:11px;text-transform:uppercase;letter-spacing:.06em;margin-bottom:5px}.value{font-size:18px;overflow-wrap:anywhere}.pending{color:var(--pending)}.complete{color:var(--accent)}.partial,.unavailable{color:var(--pending)}.note{border-left:3px solid var(--pending);background:var(--panel);padding:13px 15px;margin:12px 0}.table-wrap{overflow:auto;border:1px solid var(--line)}table{border-collapse:collapse;width:100%;min-width:860px}th,td{text-align:right;padding:10px 12px;border-bottom:1px solid var(--line);white-space:nowrap}th:first-child,td:first-child{text-align:left}th{color:var(--muted);font-size:10px;text-transform:uppercase;letter-spacing:.06em;background:var(--panel)}tr:last-child td{border-bottom:0}.empty{text-align:left;color:var(--muted)}details{border:1px solid var(--line);background:var(--panel)}summary{cursor:pointer;padding:14px}.details-body{padding:0 14px 14px}.requests{min-width:1100px}footer{margin-top:42px;color:var(--muted);font-size:12px}@media(max-width:600px){main{padding:24px 14px 60px}h1{font-size:34px}}
</style>
</head>
<body><main>
<header><div class="eyebrow">llm-slo-bench / schema v{{.Summary.SchemaVersion}}</div><h1>Inference run report</h1><div class="muted">Portable report rendered from one immutable metrics summary.</div></header>
<h2>Run metadata</h2>
<div class="grid">
<div class="cell"><span class="label">Run ID</span><span class="value">{{.Metadata.RunID}}</span></div>
<div class="cell"><span class="label">Scenario</span><span class="value">{{.Metadata.Scenario}}</span></div>
<div class="cell"><span class="label">Target</span><span class="value">{{.Metadata.Target}}</span></div>
<div class="cell"><span class="label">Model</span><span class="value">{{.Metadata.Model}}</span></div>
<div class="cell"><span class="label">Started</span><span class="value">{{.Metadata.StartedAt}}</span></div>
<div class="cell"><span class="label">Duration</span><span class="value">{{.Metadata.Duration}}</span></div>
<div class="cell"><span class="label">Config fingerprint</span><span class="value">{{.Metadata.ConfigFingerprint}}</span></div>
</div>
<h2>SLO gates</h2>
<div class="note"><strong class="pending">PENDING</strong> SLO evaluation is not attached to this metrics summary. This report does not infer or fabricate pass/fail results.</div>
<h2>Outcomes</h2>
<div class="grid">
<div class="cell"><span class="label">Scheduled</span><span class="value">{{.Summary.Counts.Scheduled}}</span></div>
<div class="cell"><span class="label">Started</span><span class="value">{{.Summary.Counts.Started}}</span></div>
<div class="cell"><span class="label">Success</span><span class="value">{{.Summary.Counts.Success}}</span></div>
<div class="cell"><span class="label">Dropped</span><span class="value">{{.Summary.Counts.Dropped}}</span></div>
<div class="cell"><span class="label">Canceled</span><span class="value">{{.Summary.Counts.Canceled}}</span></div>
<div class="cell"><span class="label">Timeout</span><span class="value">{{.Summary.Counts.Timeout}}</span></div>
<div class="cell"><span class="label">Stream error</span><span class="value">{{.Summary.Counts.StreamError}}</span></div>
<div class="cell"><span class="label">HTTP error</span><span class="value">{{.Summary.Counts.HTTPError}}</span></div>
<div class="cell"><span class="label">Error rate</span><span class="value">{{if .ZeroScheduled}}n/a{{else}}{{percent .Summary.Counts.ErrorRate}}{{end}}</span></div>
<div class="cell"><span class="label">Drop rate</span><span class="value">{{if .ZeroScheduled}}n/a{{else}}{{percent .Summary.Counts.DroppedRate}}{{end}}</span></div>
</div>
{{if .ZeroScheduled}}<div class="note">Rates are not applicable because no arrivals were scheduled.</div>{{end}}
<h2>Histogram summaries</h2>
<div class="table-wrap"><table><thead><tr><th>Metric</th><th>Count</th><th>Min</th><th>Mean</th><th>P50</th><th>P90</th><th>P95</th><th>P99</th><th>Max</th><th>Unit</th></tr></thead><tbody>
{{range .Metrics}}<tr><td>{{.Name}}</td>{{if .Summary}}<td>{{.Summary.Count}}</td><td>{{number .Summary.Min}}</td><td>{{number .Summary.Mean}}</td><td>{{number .Summary.P50}}</td><td>{{number .Summary.P90}}</td><td>{{number .Summary.P95}}</td><td>{{number .Summary.P99}}</td><td>{{number .Summary.Max}}</td><td>{{.Summary.Unit}}</td>{{else}}<td class="empty" colspan="9">no samples</td>{{end}}</tr>{{end}}
</tbody></table></div>
<h2>Usage and cost</h2>
<div class="grid">
<div class="cell"><span class="label">Usage status</span><span class="value {{.UsageState}}">{{.UsageState}}</span></div>
<div class="cell"><span class="label">Usage samples</span><span class="value">{{.Summary.Usage.Samples}}</span></div>
<div class="cell"><span class="label">Prompt tokens</span><span class="value">{{.PromptTokens}}</span></div>
<div class="cell"><span class="label">Completion tokens</span><span class="value">{{.OutputTokens}}</span></div>
<div class="cell"><span class="label">Total tokens</span><span class="value">{{.TotalTokens}}</span></div>
<div class="cell"><span class="label">Cost</span><span class="value">{{.Cost}}</span></div>
</div>
<div class="note">{{.UsageNote}}</div>
{{if .HasRequests}}<h2>Per-request details</h2><details><summary>Request records from {{.RequestSource}}</summary><div class="details-body"><div class="table-wrap"><table class="requests"><thead><tr><th>#</th><th>Outcome</th><th>HTTP</th><th>TTFB</th><th>TTFT</th><th>Chunk ITL</th><th>Duration</th><th>Usage</th><th>Prompt tokens</th><th>Completion tokens</th><th>Total tokens</th></tr></thead><tbody>{{end}}
{{end}}
{{define "request"}}<tr><td>{{.Number}}</td><td>{{.Record.Outcome}}</td><td>{{.Record.StatusCode}}</td><td>{{.TTFB}}</td><td>{{.TTFT}}</td><td>{{.ChunkITL}}</td><td>{{.Duration}}</td><td>{{.Record.UsageStatus}}</td><td>{{.PromptTokens}}</td><td>{{.CompletionTokens}}</td><td>{{.TotalTokens}}</td></tr>
{{end}}
{{define "footer"}}{{if .HasRequests}}</tbody></table></div></div></details>{{end}}
<footer>Generated by llm-slo-bench. This file contains no external assets or network requests.</footer>
</main></body></html>
{{end}}`
