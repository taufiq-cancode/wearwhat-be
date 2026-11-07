package admin

import (
	"net/http"

	"wearwhat-be/internal/app"
	"wearwhat-be/internal/httpx"
	"wearwhat-be/internal/media"
)

// Service serves the admin panel. Every route needs a signed-in admin.
type Service struct {
	*app.Deps
	Store media.Storage
}

func (s *Service) Routes(r *httpx.Router) {
	r.Admin("GET /admin/overview", s.overview)
	r.Admin("GET /admin/brands", s.brands)
	r.Admin("GET /admin/brands/{handle}", s.brand)
	r.Admin("GET /admin/brands/{handle}/document", s.document)
	r.Admin("POST /admin/brands/{handle}/verify", s.setStatus(Verified))
	r.Admin("POST /admin/brands/{handle}/reject", s.setStatus(Rejected))
	r.Admin("GET /admin/reports", s.reports)
	r.Admin("POST /admin/reports/{id}/resolve", s.resolve)
	r.Admin("GET /admin/money", s.money)
	r.Admin("POST /admin/payouts/{handle}/paid", s.payoutPaid)
	r.Admin("POST /admin/refunds/{order}/done", s.refundDone)
}

// actor is how the panel's changes are recorded.
func actor(r *http.Request) string { return "@" + httpx.Who(r).Handle }

func (s *Service) overview(_ http.ResponseWriter, r *http.Request) (any, error) {
	return GetOverview(r.Context(), s.DB)
}

func (s *Service) brands(_ http.ResponseWriter, r *http.Request) (any, error) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = InReview
	}
	return Brands(r.Context(), s.DB, status)
}

func (s *Service) brand(_ http.ResponseWriter, r *http.Request) (any, error) {
	return Brand(r.Context(), s.DB, s.Cfg.DataKey, r.PathValue("handle"))
}

func (s *Service) document(_ http.ResponseWriter, r *http.Request) (any, error) {
	return Document(r.Context(), s.DB, s.Store, s.Cfg.DataKey, r.PathValue("handle"))
}

func (s *Service) setStatus(status string) httpx.Handler {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		var in struct {
			Note string `json:"note"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return nil, err
		}
		if status == Verified {
			in.Note = ""
		}
		if _, err := SetBrandStatus(r.Context(), s.DB, actor(r), r.PathValue("handle"), status, in.Note); err != nil {
			return nil, err
		}
		return httpx.OK(), nil
	}
}

func (s *Service) reports(_ http.ResponseWriter, r *http.Request) (any, error) {
	return Reports(r.Context(), s.DB)
}

func (s *Service) resolve(_ http.ResponseWriter, r *http.Request) (any, error) {
	if err := ResolveReport(r.Context(), s.DB, actor(r), r.PathValue("id")); err != nil {
		return nil, err
	}
	return httpx.OK(), nil
}

func (s *Service) money(_ http.ResponseWriter, r *http.Request) (any, error) {
	return GetMoney(r.Context(), s.DB)
}

func (s *Service) payoutPaid(_ http.ResponseWriter, r *http.Request) (any, error) {
	var in struct {
		// Amount is the total the admin saw and sent, in cents. It must still be what's due.
		Amount *int `json:"amount"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		return nil, err
	}
	if in.Amount == nil || *in.Amount <= 0 {
		return nil, httpx.BadRequest("Send the amount you paid")
	}
	orders, total, err := MarkPayoutPaid(r.Context(), s.DB, actor(r), r.PathValue("handle"), in.Amount)
	if err != nil {
		return nil, err
	}
	return map[string]int{"orders": orders, "amount": total}, nil
}

func (s *Service) refundDone(_ http.ResponseWriter, r *http.Request) (any, error) {
	if _, err := MarkRefundDone(r.Context(), s.DB, actor(r), r.PathValue("order")); err != nil {
		return nil, err
	}
	return httpx.OK(), nil
}
