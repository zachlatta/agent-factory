package factory

import (
	"errors"
	"testing"
	"time"
)

func TestLiveCompletionMailOnce(t *testing.T) {
	a := freshCard(false, reviewLabels(map[string]string{labelStage: "landed", labelLiveMail: "pending"}))
	f, p, m := mailFactory(t, a)
	storeLog(t, f, p, "c1", "REMAINING: none")
	for i := 0; i < 3; i++ {
		if err := f.mailLiveDone(a); err != nil {
			t.Fatal(err)
		}
		a = withLabels(a, p.labels("c1"))
		f.SetAgents([]Agent{a})
	}
	if sends := m.sends(); len(sends) != 1 || sends[0].Args["subject"] != "The work is live" {
		t.Fatalf("sends=%+v", sends)
	}
}

func TestLiveCompletionMailDeliveryFailures(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "definite", true: "uncertain"}[uncertain], func(t *testing.T) {
			a := freshCard(false, reviewLabels(map[string]string{labelStage: "landed", labelLiveMail: "pending"}))
			f, p, m := mailFactory(t, a)
			storeLog(t, f, p, "c1", "REMAINING: none")
			failure := errors.New("mail unavailable")
			if uncertain {
				failure = ErrMailMaybeSent
			}
			m.errs["send_email"] = failure
			if err := f.mailLiveDone(a); err != nil && uncertain {
				t.Fatal(err)
			}
			a = withLabels(a, p.labels("c1"))
			delete(m.errs, "send_email")
			f.Now = func() time.Time { return t0.Add(mailRetryAfter + time.Second) }
			if err := f.mailLiveDone(a); err != nil {
				t.Fatal(err)
			}
			want := 2
			if uncertain {
				want = 1
			}
			if sends := m.sends(); len(sends) != want {
				t.Fatalf("sends=%+v", sends)
			}
		})
	}
}
