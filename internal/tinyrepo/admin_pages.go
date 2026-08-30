package tinyrepo

import (
	"html/template"
	"net/http"
)

// The panel's pages. One inline stylesheet, no script, no external request: the
// Content-Security-Policy the handler sets allows nothing else, and the panel
// has to stay usable on a machine that only has the repository on it.

type loginPage struct {
	Error string
	// Insecure marks a login travelling in the clear, which is worth saying on
	// the page that is about to carry a password.
	Insecure bool
}

type dashboardPage struct {
	CSRF       string
	User       string
	ConfigPath string
	Form       configForm
	Error      string
	Notice     string
	Jobs       []jobKind
	Busy       bool
	Run        *jobRun
	RunLog     string
	// Verify is set only just after the check was asked for, so the panel does
	// not show a stale answer next to a field that has since been edited.
	Verify *verifyResult
}

// packagesPage is the repository listing.
type packagesPage struct {
	CSRF   string
	User   string
	Root   string
	List   inventoryPage
	Notice string
	Error  string
}

// availablePageData is the mirror catalog page.
type availablePageData struct {
	CSRF     string
	User     string
	List     availablePage
	Declared int
	Error    string
}

type logPage struct {
	CSRF    string
	Run     *jobRun
	Log     string
	Refresh bool
}

// t exposes the existing translation table to the templates, so the panel
// speaks the same language as the command line without a second mechanism.
var adminFuncs = template.FuncMap{
	"t":     _t,
	"label": jobDescription,
	// dict is how a page passes more than one value into the shared header.
	"dict": func(pairs ...any) map[string]any {
		out := map[string]any{}
		for i := 0; i+1 < len(pairs); i += 2 {
			key, ok := pairs[i].(string)
			if !ok {
				continue
			}
			out[key] = pairs[i+1]
		}
		return out
	},
}

var adminTemplates = template.Must(template.New("admin").Funcs(adminFuncs).Parse(`
{{define "head"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
{{if .Refresh}}<meta http-equiv="refresh" content="2">{{end}}
<title>tinyrepo :: {{.Title}}</title>
<style>
:root{color-scheme:light dark;--bg:#12100e;--panel:#1a1714;--fg:#d8cfc0;--dim:#8a8176;
--accent:#ffb454;--ok:#7fd88f;--bad:#e06c75;--border:#3a332c}
@media(prefers-color-scheme:light){:root{--bg:#f5f1e8;--panel:#fff;--fg:#26221c;--dim:#6b6355;
--accent:#a35c00;--ok:#1f6f3d;--bad:#a3252c;--border:#d6cdbb}}
*{box-sizing:border-box}
body{margin:0;padding:1.5rem 1rem 4rem;background:var(--bg);color:var(--fg);
font-family:ui-monospace,"SF Mono",Menlo,Consolas,"DejaVu Sans Mono",monospace;font-size:14px;line-height:1.5}
main{max-width:78ch;margin:0 auto}
/* The table needs room a prose column does not have. min() so a narrow
   viewport still gets the full width rather than a fixed one it must scroll. */
main.wide{max-width:min(118ch,100%)}
h1{color:var(--accent);font-size:1rem;margin:0 0 .25rem}
/* The rule after a heading is drawn, not typed: a fixed run of box-drawing
   characters ends in a different place for every title length, which is what
   made the section widths look ragged. */
h2{color:var(--accent);font-size:.9rem;margin:2rem 0 .5rem;
display:flex;align-items:center;gap:.6rem}
h2::before{content:"──";color:var(--border)}
h2::after{content:"";flex:1 1 auto;border-top:1px solid var(--border)}
p{margin:0 0 .75rem}
a{color:var(--ok)}
.bar{display:flex;flex-wrap:wrap;gap:.75rem;align-items:baseline;justify-content:space-between;
padding-bottom:.5rem;margin-bottom:.5rem}
/* The panel has four pages; a single "back" link left you guessing where the
   others were. Tabs, with the current one marked, say what exists. */
.topnav{display:flex;flex-wrap:wrap;gap:.25rem;border-bottom:1px solid var(--border);
margin:0 0 1.25rem}
.topnav a{color:var(--dim);text-decoration:none;padding:.3rem .75rem;
border:1px solid transparent;border-bottom:none;white-space:nowrap}
.topnav a:hover{color:var(--accent)}
.topnav a[aria-current]{color:var(--accent);background:var(--panel);
border-color:var(--border);margin-bottom:-1px;padding-bottom:calc(.3rem + 1px)}
.dim{color:var(--dim)}
label{display:block;margin:0 0 .9rem}
label>span{display:block;color:var(--dim);font-size:.85rem;margin-bottom:.15rem}
input[type=text],input[type=password],input[type=number],textarea,select{
width:100%;background:var(--panel);color:var(--fg);border:1px solid var(--border);
padding:.35rem .5rem;font:inherit;border-radius:0}
textarea{min-height:5.5em;resize:vertical}
input:focus,textarea:focus,select:focus{outline:1px solid var(--accent);border-color:var(--accent)}
.check{display:flex;gap:.5rem;align-items:flex-start;margin:0 0 .6rem}
.check input{margin:.3rem 0 0}
.check span{color:var(--fg)}
.check em{display:block;color:var(--dim);font-style:normal;font-size:.85rem}
button{background:var(--panel);color:var(--accent);border:1px solid var(--accent);
padding:.35rem .9rem;font:inherit;cursor:pointer;border-radius:0}
button:hover:not(:disabled){background:var(--accent);color:var(--bg)}
button:disabled{color:var(--dim);border-color:var(--border);cursor:not-allowed}
.actions{display:flex;flex-wrap:wrap;gap:.5rem}
/* The scroll lives in here, on both axes, not on the page: the heading, the
   filter and both pagers stay where they are and the rows move under them, so
   the page keeps its shape however many packages there are. overscroll-behavior
   keeps a swipe that reaches the end from turning into a page scroll or a
   browser back gesture. */
.scroll{overflow:auto;overscroll-behavior:contain;margin:0 0 .75rem;
border:1px solid var(--border)}
/* Only the listing is tall enough to need capping. vh rather than a row count:
   what matters is that the pager below it stays on screen. */
.scroll.tall{max-height:min(60vh,40rem)}
.filter{display:flex;flex-wrap:wrap;gap:.5rem;align-items:center;margin:0 0 .75rem}
.filter input[type=text]{flex:1 1 16ch;width:auto}
.filter .check{margin:0}
.num{text-align:right;white-space:nowrap}
.row-action{text-align:right;padding-right:.5rem}
/* The action is the point of the row, so it must not be the thing you have to
   scroll sideways to reach: it stays pinned to the right edge of the box while
   the columns move under it. Opaque, or the text would show through. */
.scroll td.row-action,.scroll thead th:last-child{position:sticky;right:0;
background:var(--bg);box-shadow:inset 1px 0 0 var(--border)}
.scroll thead th:last-child{background:var(--panel);z-index:2}
.scroll tbody tr:hover td.row-action{background:var(--panel)}
/* The one column that can be arbitrarily long. Clipped rather than allowed to
   set the table's width, with the full text in the title attribute. */
.desc{max-width:52ch;overflow:hidden;text-overflow:ellipsis}
.row-action form{margin:0}
.row-action button{padding:.1rem .5rem;font-size:.85rem;white-space:nowrap}
button.add{color:var(--ok);border-color:var(--ok)}
button.add:hover{background:var(--ok);color:var(--bg)}
/* wrap, and every child unbreakable: on a narrow screen the position drops to
   its own centred line instead of "anterior" and "filas" splitting mid-phrase. */
.pager{display:flex;flex-wrap:wrap;gap:.4rem .75rem;align-items:center;
justify-content:space-between;border:1px solid var(--border);
padding:.4rem .75rem;margin:0 0 1rem;font-size:.9rem}
.pager>*{white-space:nowrap}
.pager .where{color:var(--dim);flex:1 1 auto;text-align:center}
.pager a{text-decoration:none}
.pager a:hover{color:var(--accent)}
.pager .off{color:var(--border)}
/* Narrow: the two controls keep the ends of the first line and the position
   moves to its own centred line under them. */
@media(max-width:600px){.pager .where{order:3;flex-basis:100%}}
table{border-collapse:collapse;width:100%}
th,td{border-bottom:1px solid var(--border);padding:.3rem .75rem;text-align:left;vertical-align:top}
th{color:var(--dim);font-weight:400;font-size:.85rem}

/* min-content on the cells and max-content on the table is what actually
   produces the scroll: without it the table squeezes itself into the container
   instead of overflowing, every column ends up a different width on every page,
   and overflow-x never fires. */
.scroll table{min-width:max-content}
.scroll th,.scroll td{white-space:nowrap}
/* 50 rows is more than a screen, so the header has to stay readable. */
.scroll thead th{position:sticky;top:0;background:var(--panel);z-index:1;
box-shadow:inset 0 -1px 0 var(--border)}
.scroll tbody tr:hover td{background:var(--panel)}
.actions form{margin:0}
pre{background:var(--panel);border:1px solid var(--border);padding:.75rem;overflow-x:auto;
max-height:26em;overflow-y:auto;margin:0 0 1rem;white-space:pre-wrap;word-break:break-word}
.msg{border-left:2px solid var(--accent);padding:.5rem .75rem;margin:0 0 1rem;background:var(--panel)}
.msg.bad{border-color:var(--bad);color:var(--bad)}
.msg.ok{border-color:var(--ok);color:var(--ok)}
.st-running{color:var(--accent)}.st-ok{color:var(--ok)}.st-failed{color:var(--bad)}
footer{border-top:1px solid var(--border);color:var(--dim);margin-top:2.5rem;padding-top:.75rem;font-size:.85rem}
</style>
</head>
<body>
<main{{if .Wide}} class="wide"{{end}}>
{{end}}

{{define "foot"}}
<footer>tinyrepo{{if .}} · {{.}}{{end}}</footer>
</main>
</body>
</html>
{{end}}

{{define "login"}}{{template "head" (dict "Title" "login" "Refresh" false)}}
<h1>tinyrepo :: {{t "panel login"}}</h1>
{{if .Error}}<p class="msg bad">{{.Error}}</p>{{end}}
{{if .Insecure}}<p class="msg">{{t "warn plain http"}}</p>{{end}}
<form method="post" action="/admin/login">
<label><span>{{t "user"}}</span>
<input type="text" name="user" autocomplete="username" autofocus required></label>
<label><span>{{t "password"}}</span>
<input type="password" name="password" autocomplete="current-password" required></label>
<button type="submit">{{t "sign in"}}</button>
</form>
{{template "foot" ""}}{{end}}

{{define "dashboard"}}{{template "head" (dict "Title" "config" "Refresh" false)}}
<div class="bar">
<h1>tinyrepo :: {{t "panel config"}}</h1>
<form method="post" action="/admin/logout">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<span class="dim">{{.User}}</span> <button type="submit">{{t "sign out"}}</button>
</form>
</div>
{{template "nav" "config"}}

{{if .Error}}<p class="msg bad">{{.Error}}</p>{{end}}
{{if .Notice}}<p class="msg ok">{{.Notice}}</p>{{end}}

<h2>{{t "actions"}}</h2>
{{if .Busy}}<p class="msg">{{t "err job busy"}} — <a href="/admin/log">{{t "view log"}}</a></p>{{end}}
<div class="actions">
{{$csrf := .CSRF}}{{$busy := .Busy}}{{range .Jobs}}
<form method="post" action="/admin/run">
<input type="hidden" name="csrf" value="{{$csrf}}">
<input type="hidden" name="job" value="{{.}}">
<button type="submit"{{if $busy}} disabled{{end}}>-{{.}} · {{label .}}</button>
</form>
{{end}}
</div>
{{if .Run}}
<p class="dim">{{t "last run"}}: -{{.Run.Kind}} ·
<span class="st-{{.Run.Status}}">{{.Run.Status}}</span> · {{.Run.Duration}} ·
<a href="/admin/log">{{t "view log"}}</a></p>
{{end}}

<h2>{{t "verify packages now"}}</h2>
<div class="actions">
<form method="post" action="/admin/verify">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<button type="submit">-vp · {{t "verify packages now"}}</button>
</form>
</div>
<p class="dim">{{t "verify hint"}}</p>
{{with .Verify}}
<p class="msg {{if .Missing}}bad{{else}}ok{{end}}">{{.Summary}}</p>
{{if .Statuses}}
<div class="scroll">
<table>
<thead><tr><th>arch</th><th>{{t "package"}}</th><th></th></tr></thead>
<tbody>
{{range .Statuses}}
<tr><td class="dim">{{.Arch}}</td><td>{{.Name}}</td>
<td class="{{if .OK}}st-ok{{else}}st-failed{{end}}">{{.Describe}}</td></tr>
{{end}}
</tbody>
</table>
</div>
{{end}}
{{end}}

<h2>{{t "configuration"}}</h2>
<p class="dim">{{.ConfigPath}}</p>
<p class="msg">{{t "warn restart"}}</p>

<form method="post" action="/admin/save">
<input type="hidden" name="csrf" value="{{.CSRF}}">
{{with .Form}}

<h2>[server]</h2>
<label><span>type</span>
<select name="type">
<option value="debian"{{if eq .Type "debian"}} selected{{end}}>debian</option>
<option value="arch"{{if eq .Type "arch"}} selected{{end}}>arch</option>
</select></label>
<label><span>source — {{t "one per line"}}</span>
<textarea name="source" spellcheck="false">{{.Source}}</textarea></label>

<h2>[destination]</h2>
<label><span>arch — {{t "one per line"}}</span>
<textarea name="arch" spellcheck="false">{{.Arch}}</textarea></label>
<label><span>path</span>
<input type="text" name="path" value="{{.Path}}" spellcheck="false"></label>
<label><span>packages — {{t "one per line"}}</span>
<textarea name="packages" spellcheck="false">{{.Packages}}</textarea></label>

<h2>[web]</h2>
<label><span>listen</span>
<input type="text" name="listen" value="{{.Listen}}" spellcheck="false"></label>
<label class="check"><input type="checkbox" name="behindProxy"{{if .BehindProxy}} checked{{end}}>
<span>behindProxy<em>{{t "help behindProxy"}}</em></span></label>
<label class="check"><input type="checkbox" name="health"{{if .Health}} checked{{end}}>
<span>health<em>{{t "help health"}}</em></span></label>
<label class="check"><input type="checkbox" name="panel"{{if .PanelOn}} checked{{end}}>
<span>config<em>{{t "help panel"}}</em></span></label>
<label><span>user</span>
<input type="text" name="user" value="{{.User}}" autocomplete="username" spellcheck="false"></label>
<label><span>{{t "new password"}}</span>
<input type="password" name="newPassword" autocomplete="new-password"
placeholder="{{t "leave blank to keep"}}"></label>

<h2>[proxy]</h2>
<label class="check"><input type="checkbox" name="proxyUse"{{if .ProxyUse}} checked{{end}}>
<span>use<em>{{t "help proxy"}}</em></span></label>
<label><span>host</span>
<input type="text" name="proxyHost" value="{{.ProxyHost}}" spellcheck="false"></label>
<label><span>port</span>
<input type="number" name="proxyPort" value="{{.ProxyPort}}" min="1" max="65535"></label>

<h2>[settings]</h2>
<label><span>maxConcurrentDownloads (1-32)</span>
<input type="number" name="maxConcurrentDownloads" value="{{.MaxConcurrentDownloads}}" min="1" max="32"></label>
<label class="check"><input type="checkbox" name="verifyChecksum"{{if .VerifyChecksum}} checked{{end}}>
<span>verifyChecksum<em>{{t "help verify"}}</em></span></label>
<label class="check"><input type="checkbox" name="skipDownloadSameSize"{{if .SkipDownloadSameSize}} checked{{end}}>
<span>skipDownloadSameSize<em>{{t "help skipsize"}}</em></span></label>
<label class="check"><input type="checkbox" name="filesDatabase"{{if .FilesDatabase}} checked{{end}}>
<span>filesDatabase<em>{{t "help filesdb"}}</em></span></label>
<label class="check"><input type="checkbox" name="onDemand"{{if .OnDemand}} checked{{end}}>
<span>onDemand<em>{{t "help ondemand"}}</em></span></label>
<label class="check"><input type="checkbox" name="debug"{{if .Debug}} checked{{end}}>
<span>debug<em>{{t "help debug"}}</em></span></label>

{{end}}
<button type="submit">{{t "save config"}}</button>
</form>
{{template "foot" .ConfigPath}}{{end}}

{{define "nav"}}<nav class="topnav">
<a href="/admin/"{{if eq . "config"}} aria-current="page"{{end}}>{{t "nav config"}}</a>
<a href="/admin/available"{{if eq . "available"}} aria-current="page"{{end}}>{{t "nav available"}}</a>
<a href="/admin/packages"{{if eq . "packages"}} aria-current="page"{{end}}>{{t "nav downloaded"}}</a>
<a href="/admin/log"{{if eq . "log"}} aria-current="page"{{end}}>{{t "nav log"}}</a>
</nav>{{end}}

{{define "pager"}}<nav class="pager">
{{if .HasPrev}}<a href="{{.PageHref .Prev}}">← {{t "previous"}}</a>{{else}}<span class="off">← {{t "previous"}}</span>{{end}}
<span class="where">{{t "page"}} {{.Page}} / {{.Pages}} · {{.Total}} {{t "rows"}}</span>
{{if .HasNext}}<a href="{{.PageHref .Next}}">{{t "next"}} →</a>{{else}}<span class="off">{{t "next"}} →</span>{{end}}
</nav>{{end}}

{{define "packages"}}{{template "head" (dict "Title" "packages" "Refresh" false "Wide" true)}}
<div class="bar">
<h1>tinyrepo :: {{t "downloaded packages"}}</h1>
</div>
{{template "nav" "packages"}}

{{if .Error}}<p class="msg bad">{{.Error}}</p>{{end}}
{{if .Notice}}<p class="msg ok">{{.Notice}}</p>{{end}}

{{with .List}}
<p class="dim">{{.Files}} {{t "files on disk"}} · {{.HumanBytes}}{{if or .Filter.Query .Filter.Undeclared}} · {{.Total}} {{t "match"}}{{end}}</p>

<form method="get" action="/admin/packages" class="filter">
<input type="text" name="q" value="{{.Filter.Query}}" placeholder="{{t "filter by name"}}" spellcheck="false">
<label class="check"><input type="checkbox" name="undeclared" value="1"{{if .Filter.Undeclared}} checked{{end}}>
<span>{{t "only undeclared"}}</span></label>
<button type="submit">{{t "filter"}}</button>
</form>

<p class="dim">{{t "declare hint"}}</p>

{{template "pager" .}}

<div class="scroll tall">
<table>
<thead><tr>
<th>{{t "package"}}</th><th>{{t "version"}}</th><th>arch</th>
<th>{{t "size"}}</th><th>{{t "modified"}}</th><th></th>
</tr></thead>
<tbody>
{{$csrf := $.CSRF}}{{$action := .PageHref .Page}}
{{range .Items}}
<tr>
<td><a href="{{.Href}}" download>{{.Name}}</a></td>
<td class="dim">{{.Version}}</td>
<td class="dim">{{.Arch}}</td>
<td class="num">{{.HumanSize}}</td>
<td class="dim">{{.When}}</td>
<td class="row-action">
<form method="post" action="{{$action}}">
<input type="hidden" name="csrf" value="{{$csrf}}">
<input type="hidden" name="name" value="{{.Name}}">
{{if .Declared}}
<input type="hidden" name="declare" value="remove">
<button type="submit" title="{{t "remove from list"}}">− {{t "declared"}}</button>
{{else}}
<input type="hidden" name="declare" value="add">
<button type="submit" class="add" title="{{t "add to list"}}">+ {{t "adopt"}}</button>
{{end}}
</form>
</td>
</tr>
{{else}}
<tr><td colspan="6" class="dim">{{t "no packages on disk"}}</td></tr>
{{end}}
</tbody>
</table>
</div>

{{template "pager" .}}
{{end}}

{{template "foot" .Root}}{{end}}

{{define "available"}}{{template "head" (dict "Title" "available" "Refresh" false "Wide" true)}}
<div class="bar">
<h1>tinyrepo :: {{t "available packages"}}</h1>
</div>
{{template "nav" "available"}}

{{if .Error}}<p class="msg bad">{{.Error}}</p>{{end}}

{{with .List}}
{{if .Stale}}
<p class="msg">{{t "err r indexes"}} — {{t "available hint"}}</p>
{{else}}
<p class="dim">{{.Catalog}} {{t "packages on the mirror"}} · {{$.Declared}} {{t "in your list"}}{{if or .Filter.Query .Filter.Declared}} · {{.Total}} {{t "match"}}{{end}}</p>

<form method="get" action="/admin/available" class="filter">
<input type="text" name="q" value="{{.Filter.Query}}" placeholder="{{t "search the mirror"}}" spellcheck="false" autofocus>
<label class="check"><input type="checkbox" name="declared" value="1"{{if .Filter.Declared}} checked{{end}}>
<span>{{t "only in my list"}}</span></label>
<button type="submit">{{t "filter"}}</button>
</form>

<p class="dim">{{t "available hint"}}</p>

{{template "availablePager" .}}

<div class="scroll tall">
<table>
<thead><tr>
<th>{{t "package"}}</th><th>{{t "version"}}</th><th>arch</th>
<th>{{t "size"}}</th><th>{{t "description"}}</th><th></th>
</tr></thead>
<tbody>
{{$csrf := $.CSRF}}{{$action := .PageHref .Page}}
{{range .Items}}
<tr>
<td>{{.Name}}</td>
<td class="dim">{{.Version}}</td>
<td class="dim">{{.Arch}}</td>
<td class="num">{{.HumanSize}}</td>
<td class="desc dim">{{.Description}}</td>
<td class="row-action">
<form method="post" action="{{$action}}">
<input type="hidden" name="csrf" value="{{$csrf}}">
<input type="hidden" name="name" value="{{.Name}}">
{{if .Declared}}
<input type="hidden" name="declare" value="remove">
<button type="submit" title="{{t "remove from list"}}">− {{t "declared"}}</button>
{{else}}
<input type="hidden" name="declare" value="add">
<button type="submit" class="add" title="{{t "add to list"}}">+ {{t "adopt"}}</button>
{{end}}
</form>
</td>
</tr>
{{else}}
<tr><td colspan="6" class="dim">{{t "nothing matches"}}</td></tr>
{{end}}
</tbody>
</table>
</div>

{{template "availablePager" .}}
{{end}}
{{end}}

{{template "foot" ""}}{{end}}

{{define "availablePager"}}<nav class="pager">
{{if .HasPrev}}<a href="{{.PageHref .Prev}}">← {{t "previous"}}</a>{{else}}<span class="off">← {{t "previous"}}</span>{{end}}
<span class="where">{{t "page"}} {{.Page}} / {{.Pages}} · {{.Total}} {{t "rows"}}</span>
{{if .HasNext}}<a href="{{.PageHref .Next}}">{{t "next"}} →</a>{{else}}<span class="off">{{t "next"}} →</span>{{end}}
</nav>{{end}}

{{define "log"}}{{template "head" (dict "Title" "log" "Refresh" .Refresh)}}
<div class="bar">
<h1>tinyrepo :: {{t "action log"}}</h1>
</div>
{{template "nav" "log"}}
{{if .Run}}
<p>-{{.Run.Kind}} · {{.Run.Description}} ·
<span class="st-{{.Run.Status}}">{{.Run.Status}}</span> · {{.Run.Duration}}</p>
{{if .Run.Err}}<p class="msg bad">{{.Run.Err.Error}}</p>{{end}}
<pre>{{.Log}}</pre>
{{if .Refresh}}<p class="dim">{{t "auto refresh"}}</p>{{end}}
{{else}}
<p class="dim">{{t "no runs yet"}}</p>
{{end}}
{{template "foot" ""}}{{end}}
`))

// render writes one page. A template failure is logged rather than partially
// sent: the status line has already gone out by then, so there is nothing
// useful left to tell the browser.
func (a *adminPanel) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	logRequest(r, status)

	if err := adminTemplates.ExecuteTemplate(w, name, data); err != nil {
		logErrorf("%s: %v", _t("err serving"), err)
	}
}
