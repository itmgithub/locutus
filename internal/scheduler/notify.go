package scheduler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"itmon.nl/locutus/internal/mail"
	"itmon.nl/locutus/internal/models"
	"itmon.nl/locutus/internal/tlsutil"
	"itmon.nl/locutus/internal/views"
)

// timeFormat is how times are written in the mails, as on the web pages.
const timeFormat = "02-01-2006 15:04:05"

// notify mails the output of a finished job to its NotifyEmail and notes in
// the output of the job whether that worked.
func (s *Scheduler) notify(ctx context.Context, jobID int64, lang string) {
	job, err := s.store.GetJob(jobID)
	if err != nil {
		log.Printf("mail job %d: %v", jobID, err)
		return
	}
	note := func(format string, args ...any) {
		if err := s.store.AppendJobOutput(job.ID, "\n"+fmt.Sprintf(format, args...)+"\n"); err != nil {
			log.Printf("mail job %d: %v", job.ID, err)
		}
	}
	cfg, err := s.store.MailConfig()
	if err != nil {
		note("The output could not be mailed: %v", err)
		return
	}
	if !cfg.Configured() {
		note("The output was not mailed to %s: no mail server is configured.", job.NotifyEmail)
		return
	}
	to, err := mail.ParseAddresses(job.NotifyEmail)
	if err != nil || len(to) == 0 {
		note("The output was not mailed: invalid address %q.", job.NotifyEmail)
		return
	}
	cfg.RootCAs = tlsutil.DefaultRootCAs()
	if err := mail.Send(ctx, cfg, s.jobMail(job, to, lang)); err != nil {
		log.Printf("mail %s of %s to %s: %v", job.Kind, job.SystemName, job.NotifyEmail, err)
		note("Mailing the output to %s failed: %v", strings.Join(to, ", "), err)
		return
	}
	note("The output has been mailed to %s.", strings.Join(to, ", "))
}

// jobMail builds the mail with the result and the output of a job, with the
// same details as the page of the job.
func (s *Scheduler) jobMail(job *models.Job, to []string, lang string) mail.Message {
	t := func(key string, args ...any) string { return s.tr.T(lang, key, args...) }
	kind, status := t("job.kind."+job.Kind), t("status."+job.Status)

	var b strings.Builder
	fmt.Fprintf(&b, "%s – %s: %s\n\n", kind, job.SystemName, status)
	row := func(label, value string) { fmt.Fprintf(&b, "%-24s %s\n", label+":", value) }
	row(t("mail.job.system"), job.SystemName)
	trigger := t("job.trigger." + job.Trigger)
	if job.Username != "" {
		trigger += " (" + job.Username + ")"
	}
	row(t("job.trigger"), trigger)
	if job.Archive != "" {
		row(t("archive.name"), job.Archive)
	}
	row(t("job.started"), job.StartedAt.Format(timeFormat))
	row(t("job.finished"), job.FinishedAt.Format(timeFormat))
	row(t("job.duration"), job.Duration().Round(time.Second).String())
	if job.Kind == models.JobRestore {
		target := job.TargetName
		if job.LocalTarget() {
			target = t("restore.target_local")
		}
		row(t("restore.target"), target)
		row(t("restore.destination"), job.Destination)
		mode, overwrite := t("restore.real_run"), t("restore.overwrite_no")
		if job.DryRun {
			mode = t("restore.dry_run_badge")
		}
		if job.Overwrite {
			overwrite = t("restore.overwrite_yes")
		}
		row(t("restore.options"), mode+" · "+overwrite)
		if !job.DryRun {
			row(t("restore.size"), views.FormatBytes(job.Bytes))
		}
	}
	heading := t("job.output")
	if job.Kind == models.JobRestore {
		heading = t("job.output_restore")
	}
	fmt.Fprintf(&b, "\n%s\n%s\n%s\n", heading, strings.Repeat("-", len([]rune(heading))), job.Output)

	return mail.Message{To: to, Subject: fmt.Sprintf("[Locutus] %s – %s: %s", kind, job.SystemName, status), Body: b.String()}
}
