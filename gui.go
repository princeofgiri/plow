package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

// GUIServer manages the web-based benchmark interface
type GUIServer struct {
	ln net.Listener

	mu        sync.Mutex
	running   bool
	requester *Requester
	report    *StreamReport
	desc      string
}

// BenchmarkRequest is the JSON payload from the web UI
type BenchmarkRequest struct {
	URL         string `json:"url"`
	Concurrency int    `json:"concurrency"`
	Duration    int    `json:"duration"` // seconds
	Method      string `json:"method"`
}

// BenchmarkStatus is returned to the web UI
type BenchmarkStatus struct {
	Running bool   `json:"running"`
	Desc    string `json:"desc"`
}

func NewGUIServer(ln net.Listener) *GUIServer {
	return &GUIServer{ln: ln}
}

func (g *GUIServer) Handler(ctx *fasthttp.RequestCtx) {
	path := string(ctx.Path())
	method := string(ctx.Method())

	ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
	ctx.Response.Header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	ctx.Response.Header.Set("Access-Control-Allow-Headers", "Content-Type")

	if method == "OPTIONS" {
		ctx.SetStatusCode(200)
		return
	}

	switch {
	case path == "/" && method == "GET":
		ctx.SetContentType("text/html; charset=utf-8")
		ctx.WriteString(guiPageHTML)

	case path == "/start" && method == "POST":
		g.handleStart(ctx)

	case path == "/stop" && method == "POST":
		g.handleStop(ctx)

	case path == "/status" && method == "GET":
		g.handleStatus(ctx)

	case strings.HasPrefix(path, "/data/") && method == "GET":
		g.handleChartData(ctx, path[len("/data/"):])

	case strings.HasPrefix(path, "/echarts/statics/"):
		ap := path[len("/echarts/statics/"):]
		f, err := assetsFS.Open(ap)
		if err != nil {
			ctx.Error(err.Error(), 404)
		} else {
			ctx.SetBodyStream(f, -1)
		}

	default:
		ctx.Error("NotFound", fasthttp.StatusNotFound)
	}
}

func (g *GUIServer) handleStart(ctx *fasthttp.RequestCtx) {
	ctx.SetContentType("application/json")

	var req BenchmarkRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		ctx.SetStatusCode(400)
		json.NewEncoder(ctx).Encode(map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if req.URL == "" {
		ctx.SetStatusCode(400)
		json.NewEncoder(ctx).Encode(map[string]string{"error": "url is required"})
		return
	}
	if req.Concurrency <= 0 {
		req.Concurrency = 1
	}
	if req.Duration <= 0 {
		req.Duration = 10
	}
	if req.Method == "" {
		req.Method = "GET"
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.running {
		ctx.SetStatusCode(409)
		json.NewEncoder(ctx).Encode(map[string]string{"error": "benchmark already running"})
		return
	}

	atomic.StoreInt64(&startTimeUnixNano, 0)

	clientOpt := &ClientOpt{
		url:      req.URL,
		method:   req.Method,
		maxConns: req.Concurrency,
	}

	dur := time.Duration(req.Duration) * time.Second
	requester, err := NewRequester(req.Concurrency, -1, dur, nil, io.Discard, clientOpt, -1)
	if err != nil {
		ctx.SetStatusCode(400)
		json.NewEncoder(ctx).Encode(map[string]string{"error": err.Error()})
		return
	}

	report := NewStreamReport()
	g.report = report
	g.requester = requester
	g.running = true
	g.desc = fmt.Sprintf("Benchmarking %s for %ds using %d connection(s)", req.URL, req.Duration, req.Concurrency)

	fmt.Fprintf(os.Stderr, "\n%s\n\n", g.desc)

	go func() {
		go requester.Run()
		go report.Collect(requester.RecordChan())

		printer := NewPrinter(-1, dur, false, false)
		printer.PrintLoop(report.Snapshot, 200*time.Millisecond, false, false, report.Done())

		g.mu.Lock()
		g.running = false
		g.mu.Unlock()

		fmt.Fprintln(os.Stderr, "\n[Benchmark complete]")
	}()

	json.NewEncoder(ctx).Encode(map[string]string{"status": "started", "desc": g.desc})
}

func (g *GUIServer) handleStop(ctx *fasthttp.RequestCtx) {
	ctx.SetContentType("application/json")
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.running || g.requester == nil {
		json.NewEncoder(ctx).Encode(map[string]string{"status": "not running"})
		return
	}
	g.requester.Cancel()
	json.NewEncoder(ctx).Encode(map[string]string{"status": "stopped"})
}

func (g *GUIServer) handleStatus(ctx *fasthttp.RequestCtx) {
	ctx.SetContentType("application/json")
	g.mu.Lock()
	running, desc := g.running, g.desc
	g.mu.Unlock()
	json.NewEncoder(ctx).Encode(BenchmarkStatus{Running: running, Desc: desc})
}

func (g *GUIServer) handleChartData(ctx *fasthttp.RequestCtx, view string) {
	ctx.SetContentType("application/json")

	g.mu.Lock()
	report := g.report
	g.mu.Unlock()

	var values []interface{}
	if report != nil {
		rd := report.Charts()
		switch view {
		case latencyView:
			if rd != nil {
				values = append(values, rd.Latency.min/1e6, rd.Latency.Mean()/1e6, rd.Latency.max/1e6)
			} else {
				values = append(values, nil, nil, nil)
			}
		case rpsView:
			if rd != nil {
				values = append(values, rd.RPS, rd.AvgRPS, rd.MaxRPS)
			} else {
				values = append(values, nil, nil, nil)
			}
		case codeView:
			if rd != nil {
				values = append(values, rd.CodeMap)
			} else {
				values = append(values, nil)
			}
		case concurrencyView:
			if rd != nil {
				values = append(values, rd.Concurrency)
			} else {
				values = append(values, nil)
			}
		}
	} else {
		switch view {
		case latencyView:
			values = append(values, nil, nil, nil)
		case rpsView:
			values = append(values, nil, nil, nil)
		default:
			values = append(values, nil)
		}
	}

	json.NewEncoder(ctx).Encode(&Metrics{
		Time:   time.Now().Format(timeFormat),
		Values: values,
	})
}

func (g *GUIServer) Serve(open bool) {
	server := fasthttp.Server{Handler: g.Handler}
	addr := "http://" + g.ln.Addr().String()
	fmt.Fprintf(os.Stderr, "🚀 Plow GUI is ready at %s\n", addr)
	fmt.Fprintln(os.Stderr, "   Open the URL above in your browser to configure and run benchmarks.")
	fmt.Fprintln(os.Stderr, "")
	if open {
		go openBrowser(addr)
	}
	_ = server.Serve(g.ln)
}

// guiPageHTML is the single-page GUI served to the browser.
const guiPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Plow — HTTP Benchmark GUI</title>
<style>
@import url('https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap');
:root {
  --bg:#0d0f17; --bg2:#13161f; --bg3:#1a1d2e;
  --card:#1e2235; --card2:#252840; --border:#2e3250;
  --accent:#6c63ff; --accent2:#9b8fff; --accent-glow:rgba(108,99,255,.25);
  --green:#2dd4a0; --red:#ff6b7a; --yellow:#fbbf24;
  --text:#e2e8f0; --text2:#94a3b8; --text3:#64748b;
  --r:12px; --rs:8px; --shd:0 8px 32px rgba(0,0,0,.5);
}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:'Inter',sans-serif;background:var(--bg);color:var(--text);min-height:100vh;line-height:1.6}

.header{background:linear-gradient(135deg,var(--bg2),var(--bg3));border-bottom:1px solid var(--border);padding:18px 36px;display:flex;align-items:center;gap:14px;position:sticky;top:0;z-index:100;backdrop-filter:blur(10px)}
.logo{font-size:26px;font-weight:700;background:linear-gradient(135deg,var(--accent),var(--accent2));-webkit-background-clip:text;-webkit-text-fill-color:transparent;background-clip:text;letter-spacing:-.5px}
.subtitle{color:var(--text3);font-size:12px}
.hstatus{margin-left:auto;display:flex;align-items:center;gap:8px;font-size:13px;color:var(--text2)}
.dot{width:8px;height:8px;border-radius:50%;background:var(--text3);transition:all .3s}
.dot.running{background:var(--green);box-shadow:0 0 8px var(--green);animation:blink 1.5s ease-in-out infinite}
@keyframes blink{0%,100%{opacity:1;transform:scale(1)}50%{opacity:.6;transform:scale(1.3)}}

.wrap{max-width:1400px;margin:0 auto;padding:28px 36px}

.card{background:var(--card);border:1px solid var(--border);border-radius:var(--r);box-shadow:var(--shd);position:relative;overflow:hidden}
.card::before{content:'';position:absolute;top:0;left:0;right:0;height:2px;background:linear-gradient(90deg,var(--accent),var(--accent2),transparent)}

/* Config */
.cfg{padding:28px 28px 24px;margin-bottom:24px}
.cfg-title{font-size:15px;font-weight:600;margin-bottom:20px;display:flex;align-items:center;gap:8px}
.cfg-title::before{content:'⚙️';font-size:17px}
.form-grid{display:grid;grid-template-columns:1fr 120px 120px 120px auto;gap:14px;align-items:end}
@media(max-width:860px){.form-grid{grid-template-columns:1fr 1fr}.btn-grp{grid-column:1/-1}}
.fg{display:flex;flex-direction:column;gap:7px}
.lbl{font-size:11px;font-weight:600;color:var(--text2);text-transform:uppercase;letter-spacing:.5px}
.inp{font-family:'Inter',sans-serif;font-size:14px;background:var(--bg2);border:1.5px solid var(--border);border-radius:var(--rs);color:var(--text);padding:9px 13px;transition:all .2s;outline:none;width:100%}
.inp:focus{border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-glow)}
.inp::placeholder{color:var(--text3)}
.btn-grp{display:flex;gap:10px;align-items:center}
.btn{font-family:'Inter',sans-serif;font-size:14px;font-weight:600;border:none;border-radius:var(--rs);padding:9px 22px;cursor:pointer;transition:all .2s;display:flex;align-items:center;gap:7px;white-space:nowrap}
.btn-run{background:linear-gradient(135deg,var(--accent),#8b5cf6);color:#fff;box-shadow:0 4px 12px var(--accent-glow)}
.btn-run:hover:not(:disabled){transform:translateY(-1px);box-shadow:0 6px 20px var(--accent-glow)}
.btn-stop{background:rgba(255,107,122,.12);color:var(--red);border:1.5px solid rgba(255,107,122,.3)}
.btn-stop:hover:not(:disabled){background:rgba(255,107,122,.22);transform:translateY(-1px)}
.btn:disabled{opacity:.35;cursor:not-allowed;transform:none!important}

/* Progress */
.prog{margin-top:18px;display:none}
.prog.show{display:block}
.prog-info{display:flex;justify-content:space-between;font-size:12px;color:var(--text2);margin-bottom:7px}
.prog-bg{background:var(--bg2);border-radius:100px;height:5px;overflow:hidden}
.prog-fill{height:100%;background:linear-gradient(90deg,var(--accent),var(--green));border-radius:100px;transition:width .4s ease;width:0%}

/* Stats – 2 baris: RPS (3 col) | Latency (3 col) */
.stats{display:grid;grid-template-columns:repeat(3,1fr);gap:14px;margin-bottom:24px}
@media(max-width:760px){.stats{grid-template-columns:repeat(2,1fr)}}
.stat{background:var(--card);border:1px solid var(--border);border-radius:var(--r);padding:18px 20px;text-align:center;transition:all .3s;position:relative;overflow:hidden}
.stat::after{content:'';position:absolute;inset:0;background:radial-gradient(circle at center,var(--accent-glow),transparent 70%);opacity:0;transition:opacity .3s}
.stat.on::after{opacity:1}
.slbl{font-size:10px;font-weight:700;text-transform:uppercase;letter-spacing:.8px;color:var(--text3);margin-bottom:7px}
.sval{font-family:'JetBrains Mono',monospace;font-size:26px;font-weight:500;line-height:1;transition:all .3s}
.sval.g{color:var(--green)}.sval.a{color:var(--accent2)}.sval.y{color:var(--yellow)}
.sunit{font-size:11px;color:var(--text3);margin-top:3px}

/* Charts */
.charts{display:grid;grid-template-columns:repeat(2,1fr);gap:18px;margin-bottom:24px}
@media(max-width:860px){.charts{grid-template-columns:1fr}}
.chart-card{background:var(--card);border:1px solid var(--border);border-radius:var(--r);overflow:hidden;box-shadow:var(--shd)}
.chart-head{padding:14px 18px 10px;border-bottom:1px solid var(--border);display:flex;align-items:center;justify-content:space-between}
.chart-title{font-size:13px;font-weight:600}
.badge{font-size:10px;padding:2px 8px;border-radius:100px;background:var(--bg3);color:var(--text3);font-weight:500}
.chart-body{padding:4px 4px 4px}

/* Log */
.log-card{background:var(--card);border:1px solid var(--border);border-radius:var(--r);overflow:hidden}
.log-head{padding:12px 18px;border-bottom:1px solid var(--border);display:flex;align-items:center;justify-content:space-between}
.log-title{font-size:13px;font-weight:600;display:flex;align-items:center;gap:7px}
.btn-xs{font-size:11px;padding:3px 10px;border-radius:5px;border:1px solid var(--border);background:var(--bg2);color:var(--text2);cursor:pointer;transition:all .2s;font-family:'Inter',sans-serif}
.btn-xs:hover{border-color:var(--accent);color:var(--accent2)}
.log-body{padding:14px 18px;font-family:'JetBrains Mono',monospace;font-size:11px;color:var(--text2);height:150px;overflow-y:auto;line-height:1.9}
.log-body::-webkit-scrollbar{width:3px}
.log-body::-webkit-scrollbar-thumb{background:var(--border);border-radius:4px}
.le{margin-bottom:1px}
.le.ok{color:var(--green)}.le.er{color:var(--red)}.le.in{color:var(--accent2)}
.le .ts{color:var(--text3);margin-right:8px}
</style>
</head>
<body>

<div class="header">
  <div>
    <div class="logo">🚀 Plow</div>
    <div class="subtitle">HTTP Load Testing Tool</div>
  </div>
  <div class="hstatus">
    <div class="dot" id="dot"></div>
    <span id="hstxt">Idle</span>
  </div>
</div>

<div class="wrap">

  <div class="card cfg">
    <div class="cfg-title">Benchmark Configuration</div>
    <div class="form-grid">
      <div class="fg">
        <label class="lbl" for="iUrl">Target URL</label>
        <input class="inp" id="iUrl" type="url" placeholder="https://example.com/api" value="" />
      </div>
      <div class="fg">
        <label class="lbl" for="iConc">Concurrency</label>
        <input class="inp" id="iConc" type="number" min="1" max="10000" value="10" />
      </div>
      <div class="fg">
        <label class="lbl" for="iDur">Duration (s)</label>
        <input class="inp" id="iDur" type="number" min="1" max="3600" value="10" />
      </div>
      <div class="fg">
        <label class="lbl" for="iMeth">Method</label>
        <select class="inp" id="iMeth">
          <option>GET</option><option>POST</option><option>PUT</option>
          <option>DELETE</option><option>PATCH</option><option>HEAD</option>
        </select>
      </div>
      <div class="btn-grp">
        <button class="btn btn-run" id="btnRun" onclick="startBench()">▶ Run Benchmark</button>
        <button class="btn btn-stop" id="btnStop" onclick="stopBench()" disabled>■ Stop</button>
      </div>
    </div>
    <div class="prog" id="prog">
      <div class="prog-info">
        <span>Running…</span><span id="ptime">0s / 10s</span>
      </div>
      <div class="prog-bg"><div class="prog-fill" id="pfill"></div></div>
    </div>
  </div>

  <div class="stats">
    <div class="stat" id="sRps"><div class="slbl">Last RPS</div><div class="sval a" id="vRps">—</div><div class="sunit">req/sec (last)</div></div>
    <div class="stat" id="sAvgRps"><div class="slbl">Average RPS</div><div class="sval a" id="vAvgRps">—</div><div class="sunit">req/sec (avg)</div></div>
    <div class="stat" id="sMaxRps"><div class="slbl">Max RPS</div><div class="sval a" id="vMaxRps">—</div><div class="sunit">req/sec (peak)</div></div>
    <div class="stat" id="sLat"><div class="slbl">Avg Latency</div><div class="sval g" id="vLat">—</div><div class="sunit">ms</div></div>
    <div class="stat" id="sMin"><div class="slbl">Min Latency</div><div class="sval" id="vMin">—</div><div class="sunit">ms</div></div>
    <div class="stat" id="sMax"><div class="slbl">Max Latency</div><div class="sval y" id="vMax">—</div><div class="sunit">ms</div></div>
  </div>

  <div class="charts">
    <div class="chart-card">
      <div class="chart-head"><div class="chart-title">Latency (ms)</div><div class="badge">realtime</div></div>
      <div class="chart-body"><div id="cLatency" style="height:220px"></div></div>
    </div>
    <div class="chart-card">
      <div class="chart-head"><div class="chart-title">Requests / Second</div><div class="badge">realtime</div></div>
      <div class="chart-body"><div id="cRps" style="height:220px"></div></div>
    </div>
    <div class="chart-card">
      <div class="chart-head"><div class="chart-title">HTTP Status Codes</div><div class="badge">realtime</div></div>
      <div class="chart-body"><div id="cCode" style="height:220px"></div></div>
    </div>
    <div class="chart-card">
      <div class="chart-head"><div class="chart-title">Concurrency</div><div class="badge">realtime</div></div>
      <div class="chart-body"><div id="cConc" style="height:220px"></div></div>
    </div>
  </div>

  <div class="log-card">
    <div class="log-head">
      <div class="log-title">📋 Activity Log</div>
      <button class="btn-xs" onclick="clearLog()">Clear</button>
    </div>
    <div class="log-body" id="logBody">
      <div class="le in"><span class="ts">—</span>Plow GUI ready. Configure your benchmark and click Run.</div>
    </div>
  </div>

</div>

<script src="/echarts/statics/echarts.min.js"></script>
<script>
// ────────────────────────────────────────────────────────────────────────────
// COLORS
// ────────────────────────────────────────────────────────────────────────────
const C = {
  accent:'#6c63ff', accent2:'#9b8fff',
  green:'#2dd4a0',  yellow:'#fbbf24', red:'#ff6b7a',
  border:'#2e3250', text2:'#94a3b8',
};

const MAX = 120;

// ────────────────────────────────────────────────────────────────────────────
// LOCAL DATA STORE — we never call getOption() to retrieve series data back,
// because echarts wraps everything in nested arrays which causes bugs.
// Instead we maintain JS arrays here and pass them on every setOption call.
// ────────────────────────────────────────────────────────────────────────────
const D = {
  latency:     { x:[], mn:[], mean:[], mx:[] },
  rps:         { x:[], v:[] },
  code:        { x:[], s:{} },           // s = { '200': [...], ... }
  concurrency: { x:[], v:[] },
};

function trim(a){ while(a.length > MAX) a.shift(); }

// ────────────────────────────────────────────────────────────────────────────
// CHART INIT — animation off for smooth realtime feel
// ────────────────────────────────────────────────────────────────────────────
function mkBase(legend){
  return {
    animation: false,
    backgroundColor: 'transparent',
    grid:{ top: legend?38:22, right:16, bottom:34, left:62 },
    tooltip:{ trigger:'axis', backgroundColor:'#252840', borderColor:'#2e3250',
              textStyle:{ color:'#e2e8f0', fontSize:12 } },
    xAxis:{ type:'category', data:[], boundaryGap:false,
            axisLine:{ lineStyle:{ color:C.border } },
            axisLabel:{ color:C.text2, fontSize:11 },
            splitLine:{ show:false } },
    yAxis:{ type:'value', scale:true,
            axisLine:{ show:false }, axisTick:{ show:false },
            axisLabel:{ color:C.text2, fontSize:11 },
            splitLine:{ lineStyle:{ color:C.border, type:'dashed' } } },
    legend: legend ? { top:6, right:8,
              textStyle:{ color:C.text2, fontSize:11 },
              itemWidth:14, itemHeight:2 } : undefined,
  };
}

function mkSeries(name, color, area){
  return {
    name, type:'line', data:[], smooth:true, symbol:'none',
    lineStyle:{ width:2, color },
    areaStyle: area ? { color:{ type:'linear',x:0,y:0,x2:0,y2:1,
      colorStops:[{offset:0,color:color+'44'},{offset:1,color:color+'00'}] } } : undefined,
    itemStyle:{ color },
  };
}

const EC = {
  lat: echarts.init(document.getElementById('cLatency')),
  rps: echarts.init(document.getElementById('cRps')),
  cod: echarts.init(document.getElementById('cCode')),
  con: echarts.init(document.getElementById('cConc')),
};

EC.lat.setOption({ ...mkBase(true),  series:[mkSeries('Min',C.green,false), mkSeries('Mean',C.accent2,true), mkSeries('Max',C.yellow,false)] });
EC.rps.setOption({ ...mkBase(false), series:[mkSeries('RPS',C.accent,true)] });
EC.cod.setOption({ ...mkBase(false), series:[mkSeries('200',C.green,false)] });
EC.con.setOption({ ...mkBase(false), series:[mkSeries('Concurrency',C.yellow,true)] });

window.addEventListener('resize', ()=>{ Object.values(EC).forEach(c=>c.resize()); });

// ────────────────────────────────────────────────────────────────────────────
// UPDATE HELPERS — always use local D arrays, never getOption()
// ────────────────────────────────────────────────────────────────────────────
function updateLatency(t, mn, mean, mx){
  D.latency.x.push(t);    trim(D.latency.x);
  D.latency.mn.push(mn);  trim(D.latency.mn);
  D.latency.mean.push(mean); trim(D.latency.mean);
  D.latency.mx.push(mx);  trim(D.latency.mx);
  EC.lat.setOption({ xAxis:{ data:D.latency.x },
    series:[{name:'Min',data:D.latency.mn},{name:'Mean',data:D.latency.mean},{name:'Max',data:D.latency.mx}] });
}

function updateRps(t, v){
  D.rps.x.push(t); trim(D.rps.x);
  D.rps.v.push(v); trim(D.rps.v);
  EC.rps.setOption({ xAxis:{ data:D.rps.x }, series:[{name:'RPS',data:D.rps.v}] });
}

function updateCode(t, codesObj){
  D.code.x.push(t); trim(D.code.x);
  const known = D.code.s;

  if(codesObj === null){
    for(const k in known){ known[k].push(null); trim(known[k]); }
  } else {
    // ensure 200 always exists
    if(!('200' in codesObj)) codesObj['200'] = null;
    for(const code in codesObj){
      if(!(code in known)){
        // backfill with nulls for previous ticks
        known[code] = new Array(D.code.x.length - 1).fill(null);
      }
      known[code].push(codesObj[code]);
      trim(known[code]);
    }
    // pad missing codes this tick
    for(const k in known){
      if(!(k in codesObj)){ known[k].push(null); trim(known[k]); }
    }
  }

  const cc = {'2':C.green,'4':C.yellow,'5':C.red};
  const series = Object.keys(known).map(code => ({
    name: code, type:'line', smooth:true, symbol:'none',
    data: known[code],
    lineStyle:{ width:2, color:cc[code[0]]||C.accent2 },
    itemStyle:{ color:cc[code[0]]||C.accent2 },
  }));
  EC.cod.setOption({ xAxis:{ data:D.code.x }, series }, false);
}

function updateConc(t, v){
  D.concurrency.x.push(t); trim(D.concurrency.x);
  D.concurrency.v.push(v); trim(D.concurrency.v);
  EC.con.setOption({ xAxis:{ data:D.concurrency.x }, series:[{name:'Concurrency',data:D.concurrency.v}] });
}

// ────────────────────────────────────────────────────────────────────────────
// STATE
// ────────────────────────────────────────────────────────────────────────────
let running = false, pollTmr = null, progTmr = null;
let startedAt = 0, targetDur = 10;

// ────────────────────────────────────────────────────────────────────────────
// CONTROLS
// ────────────────────────────────────────────────────────────────────────────
async function startBench(){
  const url  = document.getElementById('iUrl').value.trim();
  const conc = parseInt(document.getElementById('iConc').value)||10;
  const dur  = parseInt(document.getElementById('iDur').value)||10;
  const meth = document.getElementById('iMeth').value;

  if(!url){ addLog('er','Please enter a target URL'); document.getElementById('iUrl').focus(); return; }
  try{ new URL(url); } catch{ addLog('er','Invalid URL — must start with http:// or https://'); return; }

  targetDur = dur; startedAt = Date.now();
  resetCharts();

  try{
    const r = await fetch('/start',{ method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({url,concurrency:conc,duration:dur,method:meth}) });
    const d = await r.json();
    if(!r.ok){ addLog('er','Error: '+(d.error||r.statusText)); return; }
    setRunning(true);
    addLog('in','▶ '+d.desc);
    startPoll(); startProg();
  } catch(e){ addLog('er','Network error: '+e.message); }
}

async function stopBench(){
  try{ await fetch('/stop',{method:'POST'}); addLog('in','■ Stop signal sent'); }
  catch(e){ addLog('er','Failed to stop: '+e.message); }
}

function setRunning(r){
  running = r;
  document.getElementById('btnRun').disabled  = r;
  document.getElementById('btnStop').disabled = !r;
  document.getElementById('dot').className    = 'dot'+(r?' running':'');
  document.getElementById('hstxt').textContent = r ? 'Running…' : 'Idle';
  document.getElementById('prog').className   = 'prog'+(r?' show':'');
  if(!r) document.getElementById('pfill').style.width = '0%';
  ['sRps','sAvgRps','sMaxRps','sLat','sMin','sMax'].forEach(id=>
    document.getElementById(id).classList.toggle('on',r));
}

// ────────────────────────────────────────────────────────────────────────────
// POLLING
// ────────────────────────────────────────────────────────────────────────────
function startPoll(){
  if(pollTmr) clearInterval(pollTmr);
  pollAll();
  pollTmr = setInterval(pollAll, 1000);
}
function stopPoll(){ if(pollTmr) clearInterval(pollTmr); pollTmr=null; }

async function pollAll(){
  try{
    const r = await fetch('/status');
    const s = await r.json();
    if(!s.running && running){
      await fetchViews();
      setRunning(false); stopPoll(); stopProg();
      addLog('ok','✓ Benchmark completed!');
      return;
    }
  } catch{}
  await fetchViews();
}

async function fetchViews(){
  await Promise.all(['latency','rps','code','concurrency'].map(v=>fetchView(v)));
}

async function fetchView(view){
  try{
    const r = await fetch('/data/'+view);
    if(!r.ok) return;
    const d = await r.json();
    const t = d.time, v = d.values;

    if(view==='latency'){
      const [mn,mean,mx] = [v[0],v[1],v[2]];
      updateLatency(t, mn, mean, mx);
      setText('vLat', mean!=null ? mean.toFixed(2) : '—');
      setText('vMin', mn  !=null ? mn.toFixed(2)   : '—');
      setText('vMax', mx  !=null ? mx.toFixed(2)   : '—');
    } else if(view==='rps'){
      const [cur, avg, mx] = [v[0], v[1], v[2]];
      updateRps(t, cur);
      setText('vRps',    cur!=null ? Math.round(cur) : '—');
      setText('vAvgRps', avg!=null ? Math.round(avg) : '—');
      setText('vMaxRps', mx !=null ? Math.round(mx)  : '—');
    } else if(view==='code'){
      updateCode(t, v[0]);
    } else if(view==='concurrency'){
      updateConc(t, v[0]);
    }
  } catch{}
}

// ────────────────────────────────────────────────────────────────────────────
// PROGRESS BAR
// ────────────────────────────────────────────────────────────────────────────
function startProg(){
  if(progTmr) clearInterval(progTmr);
  progTmr = setInterval(()=>{
    const e = (Date.now()-startedAt)/1000;
    const p = Math.min(100,(e/targetDur)*100);
    document.getElementById('pfill').style.width = p+'%';
    document.getElementById('ptime').textContent = Math.floor(e)+'s / '+targetDur+'s';
  },200);
}
function stopProg(){ if(progTmr) clearInterval(progTmr); progTmr=null; }

// ────────────────────────────────────────────────────────────────────────────
// HELPERS
// ────────────────────────────────────────────────────────────────────────────
function resetCharts(){
  D.latency     = { x:[], mn:[], mean:[], mx:[] };
  D.rps         = { x:[], v:[] };
  D.code        = { x:[], s:{} };
  D.concurrency = { x:[], v:[] };

  EC.lat.setOption({ xAxis:{data:[]}, series:[{name:'Min',data:[]},{name:'Mean',data:[]},{name:'Max',data:[]}] }, false);
  EC.rps.setOption({ xAxis:{data:[]}, series:[{name:'RPS',data:[]}] }, false);
  EC.cod.setOption({ xAxis:{data:[]}, series:[{name:'200',data:[]}] }, false);
  EC.con.setOption({ xAxis:{data:[]}, series:[{name:'Concurrency',data:[]}] }, false);

  ['vRps','vAvgRps','vMaxRps','vLat','vMin','vMax'].forEach(id=>setText(id,'—'));
}

function setText(id, txt){ document.getElementById(id).textContent = txt; }

function esc(s){ return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }

function addLog(type, msg){
  const b = document.getElementById('logBody');
  const e = document.createElement('div');
  e.className = 'le '+type;
  const t = new Date().toLocaleTimeString();
  e.innerHTML = '<span class="ts">['+t+']</span>'+esc(msg);
  b.appendChild(e);
  b.scrollTop = b.scrollHeight;
}

function clearLog(){ document.getElementById('logBody').innerHTML = ''; }

// ────────────────────────────────────────────────────────────────────────────
// ON LOAD — check if benchmark already running (e.g. page refresh)
// ────────────────────────────────────────────────────────────────────────────
window.addEventListener('load', async ()=>{
  try{
    const r = await fetch('/status');
    const s = await r.json();
    if(s.running){
      setRunning(true);
      addLog('in','Benchmark in progress: '+s.desc);
      startPoll(); startProg();
    }
  } catch{}
});
</script>
</body>
</html>`
