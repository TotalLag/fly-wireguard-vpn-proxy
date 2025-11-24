package ui

import "html/template"

var Page = template.Must(template.New("page").Parse(`<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Your WireGuard VPN</title>
    <style>
      body { font-family: system-ui, -apple-system, BlinkMacSystemFont, sans-serif; max-width: 800px; margin: 2rem auto; padding: 0 1rem; }
      pre { background: #f5f5f5; padding: 1rem; overflow-x: auto; }
      img { border: 1px solid #ddd; padding: 0.5rem; background: #fff; max-width: 100%; height: auto; }
    </style>
  </head>
  <body>
    <h1>Your WireGuard VPN</h1>

    <h2>1. Scan this QR code with the WireGuard mobile app</h2>
    <p>Open the WireGuard app on your phone and choose "Scan from QR code".</p>
    <img src="data:image/png;base64,{{.QRBase64}}" alt="WireGuard config QR">

    <h2>2. Or copy this configuration into a desktop client</h2>
    <pre>{{.Config}}</pre>

    <p><strong>Note:</strong> This page is one-time only. After you close it, the bootstrap endpoint is disabled.</p>
  </body>
</html>
`))