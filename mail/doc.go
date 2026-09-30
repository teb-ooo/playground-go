// Package mail is the only way playground components send email. It speaks the
// HTTP APIs of Resend and Postmark, renders text and HTML pairs from Go
// templates layered on an embedded base layout, and on staging rewrites every
// recipient to MAIL_STAGING_SINK and prefixes the subject with
// "[staging <app>]" so agents can exercise email without reaching real
// people. Message bodies are never logged.
//
// # Templates
//
// The base layout (templates/base.html.tmpl and templates/base.txt.tmpl) is
// owned by the @teb-ooo/ui package's email/ directory; playground-go embeds a
// copy so the Go binary builds without npm. Placeholder contract: the base
// uses {{.Title}}, {{.Preheader}}, {{.PlaygroundName}}, {{.Footer}} and
// {{template "content" .}}. An app template <name>.html.tmpl and
// <name>.txt.tmpl (in the fs.FS given as Config.Templates, usually the
// app's internal/mail/ directory) defines "content"; inside it the app's own
// data is available as .Data.
package mail
