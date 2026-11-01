package httpapi_test

import "testing"

// loaderStub is a head the layout loader can append to: links are kept in
// document order, customLink is the operator's sheet (null when there is none),
// and pageLink is the sheet the server put in the document.
const loaderStub = `
	var links = [], customLink = null, pageLink = null, created = [];
	doc.head = {insertBefore: function (n, ref) {
		var i = ref ? links.indexOf(ref) : -1;
		if (i < 0) links.push(n); else links.splice(i, 0, n);
	}};
	doc.createElement = function (tag) {
		var l = {tag: tag, attrs: {}, setAttribute: function (k, v) { this.attrs[k] = v; }, getAttribute: function (k) { return this.attrs[k]; },
			remove: function () { var i = links.indexOf(this); if (i >= 0) links.splice(i, 1); }};
		created.push(l);
		return l;
	};
	var plainQuery = doc.querySelector;
	doc.querySelector = function (s) {
		if (s === 'link[data-hfy-custom]') return customLink;
		if (s === 'link[data-hfy-layout]') return pageLink;
		return plainQuery(s);
	};
	var URLS = {transcript: '/embed/assets/layouts/transcript.aaaaaaaaaaaa.css', whatsapp: '/embed/assets/layouts/whatsapp.bbbbbbbbbbbb.css'};
	function tick() { return new Promise(function (r) { setTimeout(r, 0); }); }
	function run(f) { f().catch(function (e) { console.error(e.stack || e); process.exit(1); }); }
`

func runLoader(t *testing.T, body string) {
	t.Helper()
	runNode(t, loaderStub+"run(async function () {\n"+body+"\n});")
}

// configure can only pick a layout the page's map names: anything else is
// ignored, and no link, so no request, is made for it.
func TestConfigureLayoutIgnoresUnknown(t *testing.T) {
	runLoader(t, `
		var c = boot({layout: 'bubbles'});
		var bad = ['x', '', '__proto__', 'constructor', 'toString', 'hasOwnProperty', '../ui', '/etc/passwd', 'http://evil.example/x.css',
			'whatsapp ', 'WHATSAPP', ['whatsapp'], {}, 5, null, true, undefined];
		bad.forEach(function (name) { c.applyConfigure({layout: name}); });
		await tick();
		eq(created.length, 0, 'no link was created');
		eq(c.config.layout, 'bubbles');
		eq(el.dataset.layout, 'bubbles');
		// a map in the configure message itself is not read
		c.applyConfigure({layout: 'evil', layouts: {evil: 'http://evil.example/x.css'}});
		await tick();
		eq(created.length, 0, 'configure cannot add layouts');
		eq(Object.keys(c.config.layouts).sort(), ['transcript', 'whatsapp']);
		// a known layout still loads from the page's URL, whatever else the message carries
		c.applyConfigure({layout: 'whatsapp', href: 'http://evil.example/x.css', url: 'http://evil.example/y.css', css: 'http://evil.example/z.css'});
		eq(created.length, 1);
		eq(created[0].href, URLS.whatsapp);
	`)
}

// The page config is the server's, but it is read as strictly: a layout it
// names that its map lacks falls back to the default, and keys like __proto__ are plain.
func TestPageLayoutMapIsStrict(t *testing.T) {
	runLoader(t, `
		eq(boot({layout: 'evil'}).config.layout, 'bubbles');
		eq(boot({layout: '__proto__', layouts: JSON.parse('{"__proto__": "/embed/a.css"}')}).config.layout, '__proto__', 'a map key is a name, nothing more');
		eq(Object.getPrototypeOf(api.resolve().layouts), null);
		eq(boot({layout: 'toString'}).config.layout, 'bubbles');
		eq(boot({layout: 'whatsapp'}).config.layout, 'whatsapp');
		eq(boot({layout: 'whatsapp', layouts: null}).config.layout, 'bubbles', 'no map, no layouts');
		eq(boot({layout: 'bubbles', layouts: 'whatsapp'}).config.layout, 'bubbles');
	`)
}

// A layout named by the map is fetched, and applied once its sheet is in:
// until then the current layout stays. The link goes before the operator's sheet.
func TestConfigureLayoutLoadsLazily(t *testing.T) {
	runLoader(t, `
		customLink = {tag: 'custom', attrs: {}};
		links.push(customLink);
		var c = boot({layout: 'bubbles'});
		c.applyConfigure({layout: 'whatsapp'});
		eq(created.length, 1);
		var l = created[0];
		eq([l.tag, l.rel, l.href, l.attrs['data-hfy-layout']], ['link', 'stylesheet', URLS.whatsapp, 'whatsapp']);
		eq(links.indexOf(l), 0, 'before the operator sheet');
		eq(c.config.layout, 'bubbles', 'not applied before the sheet loads');
		eq(el.dataset.layout, 'bubbles');
		l.onload();
		await tick();
		eq(c.config.layout, 'whatsapp');
		eq(el.dataset.layout, 'whatsapp');
		// back to the default is immediate; to a loaded layout needs no second link
		c.applyConfigure({layout: 'bubbles'});
		eq(el.dataset.layout, 'bubbles');
		c.applyConfigure({layout: 'whatsapp'});
		await tick();
		eq(created.length, 1, 'the sheet is fetched once');
		eq(el.dataset.layout, 'whatsapp');
	`)
}

// A host that repeats the layout the page already has (the usual configure on
// every load) costs no second link.
func TestConfigureSameLayoutLoadsNothing(t *testing.T) {
	runLoader(t, `
		var c = boot({layout: 'whatsapp'});
		c.applyConfigure({layout: 'whatsapp', theme: 'dark'});
		await tick();
		eq(created.length, 0);
		eq(el.dataset.layout, 'whatsapp');
		eq(el.dataset.theme, 'dark');
	`)
}

// A sheet that does not load (404, network) leaves the UI as it was, takes its
// link out, and a later configure tries again.
func TestConfigureLayoutFailureKeepsCurrent(t *testing.T) {
	runLoader(t, `
		var c = boot({layout: 'transcript'});
		c.applyConfigure({layout: 'whatsapp'});
		created[0].onerror();
		await tick();
		eq(c.config.layout, 'transcript');
		eq(el.dataset.layout, 'transcript');
		eq(links.length, 0, 'the failed link is removed');
		c.applyConfigure({theme: 'dark'});
		eq(el.dataset.theme, 'dark', 'other settings still apply');
		c.applyConfigure({layout: 'whatsapp'});
		eq(created.length, 2, 'a later configure retries');
		created[1].onload();
		await tick();
		eq(el.dataset.layout, 'whatsapp');
	`)
}

// Layouts switched faster than their sheets load: the last request wins, in
// whichever order the sheets arrive.
func TestConfigureLayoutConcurrentSwitches(t *testing.T) {
	for name, body := range map[string]string{
		"first sheet arrives first": `created[0].onload(); await tick(); created[1].onload();`,
		"last sheet arrives first":  `created[1].onload(); await tick(); created[0].onload();`,
	} {
		t.Run(name, func(t *testing.T) {
			runLoader(t, `
				var c = boot({layout: 'bubbles'});
				c.applyConfigure({layout: 'whatsapp'});
				c.applyConfigure({layout: 'transcript'});
				eq(created.length, 2);
				`+body+`
				await tick();
				eq(c.config.layout, 'transcript');
				eq(el.dataset.layout, 'transcript');
			`)
		})
	}
	t.Run("default requested while a sheet loads", func(t *testing.T) {
		runLoader(t, `
			var c = boot({layout: 'bubbles'});
			c.applyConfigure({layout: 'whatsapp'});
			c.applyConfigure({layout: 'bubbles'});
			created[0].onload();
			await tick();
			eq(el.dataset.layout, 'bubbles', 'a late sheet does not undo a later choice');
		`)
	})
	t.Run("same layout twice", func(t *testing.T) {
		runLoader(t, `
			var c = boot({layout: 'bubbles'});
			c.applyConfigure({layout: 'whatsapp'});
			c.applyConfigure({layout: 'whatsapp'});
			eq(created.length, 1);
			created[0].onload();
			await tick();
			eq(el.dataset.layout, 'whatsapp');
		`)
	})
}

// The sheet the document links settles before the widget starts. One that did
// not load leaves the default layout, so the UI is the usable bubbles one, not
// a half-styled other layout.
func TestInitFallsBackWhenTheLayoutSheetFailed(t *testing.T) {
	rules := func(n string) string {
		return `{sheet: {cssRules: {length: ` + n + `}}, getAttribute: function () { return 'whatsapp'; }}`
	}
	for _, tc := range []struct{ name, link, layout, want string }{
		{"loaded", rules("14"), "whatsapp", "whatsapp"},
		{"no sheet", `{sheet: null, getAttribute: function () { return 'whatsapp'; }}`, "whatsapp", "bubbles"},
		{"empty sheet (what Chrome gives a 404)", rules("0"), "whatsapp", "bubbles"},
		{"opaque sheet (what Chrome gives a network error)", `{sheet: {get cssRules() { throw new Error('SecurityError'); }}, getAttribute: function () { return 'whatsapp'; }}`, "whatsapp", "bubbles"},
		{"no link, default layout", `null`, "bubbles", "bubbles"},
		{"a failed sheet of another layout than the one in use", `{sheet: null, getAttribute: function () { return 'transcript'; }}`, "whatsapp", "whatsapp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runLoader(t, `
				pageLink = `+tc.link+`;
				win.__HFY_EMBED__ = {layout: '`+tc.layout+`', layouts: URLS};
				var c = api.hfyEmbed();
				c.init().catch(function () {}); // runs on to the network; the layout is settled before that
				eq(c.config.layout, '`+tc.want+`');
				eq(el.dataset.layout, '`+tc.want+`');
			`)
		})
	}
}
