package service

import "testing"

func TestTaskURLIsCanonicalTicketLink(t *testing.T) {
	rel := &Service{}
	if got := rel.TaskURL(7, 34); got != "/t/34" {
		t.Fatalf("TaskURL without an app URL = %q, want /t/34", got)
	}
	abs := &Service{AppURL: "https://amp.example.com/"}
	if got := abs.TaskURL(7, 34); got != "https://amp.example.com/t/34" {
		t.Fatalf("TaskURL = %q, want https://amp.example.com/t/34", got)
	}
	if got := abs.TicketURL("ba", 34); got != "https://amp.example.com/t/BA-34" {
		t.Fatalf("TicketURL = %q, want https://amp.example.com/t/BA-34", got)
	}
	if got := abs.TicketURL(" ", 34); got != "https://amp.example.com/t/34" {
		t.Fatalf("TicketURL without a code = %q, want the /t/<id> form", got)
	}
	if got := abs.RunURL(12); got != "https://amp.example.com/r/12" {
		t.Fatalf("RunURL = %q, want https://amp.example.com/r/12", got)
	}
}
