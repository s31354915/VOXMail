package calls

import (
	"fmt"
	"strings"
)

// Readback gives callers a way to check typed and dictated text before send.
// Bound the body as on received mail, and explicitly announce any shortening.
func (s *Service) draftReviewPrompt(sess *session) string {
	s.mu.Lock()
	d := sess.Draft
	to := append([]string{d.To}, d.AdditionalTo...)
	cc, bcc := append([]string(nil), d.Cc...), append([]string(nil), d.Bcc...)
	s.mu.Unlock()
	parts := []string{"Message preview. To: " + speakable(strings.Join(to, ", "), 600) + "."}
	if len(cc) > 0 {
		parts = append(parts, "Copy to: "+speakable(strings.Join(cc, ", "), 600)+".")
	}
	if len(bcc) > 0 {
		parts = append(parts, "Hidden copy to: "+speakable(strings.Join(bcc, ", "), 600)+".")
	}
	subject := strings.TrimSpace(d.Subject)
	if subject == "" {
		subject = "blank"
	}
	body := strings.TrimSpace(d.Body)
	if body == "" {
		body = "blank"
	}
	parts = append(parts, "Subject: "+speakable(subject, maxIVRSubjectRunes)+".", "Message: "+speakable(body, maxSpokenBodyRunes)+".")
	if len(d.Attachments) > 0 {
		parts = append(parts, fmt.Sprintf("Attachments: %d.", len(d.Attachments)))
	}
	parts = append(parts, menuPrompt("review"))
	return strings.Join(parts, " ")
}
