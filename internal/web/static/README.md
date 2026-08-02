# Vendored files

`htmx.min.js` and `mathjax-tex-svg.js` (plus `input/tex/extensions/`) are
committed here rather than pulled from a CDN, so the app never depends on a
third party being reachable, and never leaks a reader's activity to one
either.

## Updating MathJax

```
npm pack mathjax@3
tar -xzf mathjax-*.tgz
cp package/es5/tex-svg.js static/mathjax-tex-svg.js
rm -rf static/input/tex/extensions
cp -r package/es5/input/tex/extensions static/input/tex/extensions
```

The `extensions/` directory matters: `tex-svg.js` is not fully self-contained
on its own — it lazy-loads a handful of less common TeX packages (`\enclose`,
`\cancel`, `mhchem`, ...) at `input/tex/extensions/*.js`, resolved relative to
wherever `tex-svg.js` itself was loaded from. Vendoring only the one file
looks like it works — everyday `\frac`, `\int`, `\partial` and so on are all
baked into the main bundle — until an article happens to use one of the
lazy-loaded packages, and that equation silently fails to render instead.
