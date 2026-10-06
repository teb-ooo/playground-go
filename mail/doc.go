// Package mail is the only way playground components send email, and it is
// transport only. It speaks the HTTP APIs of Resend and Postmark and sends the
// Message it is given: recipients, subject, and a plain text and/or HTML body.
// On staging it rewrites every recipient to MAIL_STAGING_SINK and prefixes the
// subject with "[staging <app>]" so agents can exercise email without reaching
// real users. Message bodies are never logged.
//
// # Templates belong to the app
//
// This package has no layout, no template engine and no wordmark: from v0.4.0
// the embedded base layout is gone. Each app owns its email templates as
// complete text and HTML documents in its own repository (for example
// internal/mail/), renders them with its own code (text/template and
// html/template) and passes the results to Mailer.Send as Message.Text and
// Message.HTML. Send at least a text body; add HTML only if the email needs
// an action button.
package mail
