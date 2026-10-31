package dashboard

import "net/http"

// ManagementAsset serves only the login shell. The caller owns authentication
// for every data/control route; embedding assets grants no runtime authority.
func ManagementAsset(w http.ResponseWriter, r *http.Request) {
	name, kind := "", ""
	switch r.URL.Path {
	case "/manage/":
		name, kind = "manage.html", "text/html; charset=utf-8"
	case "/manage/manage.js":
		name, kind = "manage.js", "text/javascript; charset=utf-8"
	case "/manage/manage-setup.js":
		name, kind = "manage-setup.js", "text/javascript; charset=utf-8"
	case "/manage/manage-monitors.js":
		name, kind = "manage-monitors.js", "text/javascript; charset=utf-8"
	case "/manage/manage.css":
		name, kind = "manage.css", "text/css; charset=utf-8"
	case "/manage/signal.css":
		name, kind = "signal.css", "text/css; charset=utf-8"
	case "/manage/signal.js":
		name, kind = "signal.js", "text/javascript; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	b, err := assets.ReadFile(name)
	if err != nil {
		http.Error(w, "Asset unavailable.", 500)
		return
	}
	w.Header().Set("Content-Type", kind)
	w.Write(b)
}
