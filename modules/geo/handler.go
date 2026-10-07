package geo

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/search"
)

// item serializes a record with the extra top-level key distance_km.
type item struct {
	rec  *core.Record
	dist float64
	has  bool
}

func (i item) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(i.rec)
	if err != nil || !i.has || len(b) < 2 || b[len(b)-1] != '}' {
		return b, err
	}
	d, err := json.Marshal(i.dist)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(b)+len(d)+16)
	out = append(out, b[:len(b)-1]...)
	if len(b) > 2 {
		out = append(out, ',')
	}
	out = append(out, `"distance_km":`...)
	out = append(out, d...)
	return append(out, '}'), nil
}

func paramErr(e *core.RequestEvent, pe *paramError) error {
	return e.BadRequestError("Failed to load the records.", validation.Errors{
		pe.param: validation.NewError(ErrCode, pe.msg),
	})
}

func parseQuery(q url.Values, col *core.Collection, superuser bool) (*spec, error) {
	s := &spec{col: col}
	nearRaw, bboxRaw := q.Get("near"), q.Get("bbox")
	if nearRaw == "" && bboxRaw == "" {
		return nil, bad("near", "one of near or bbox is required.")
	}
	if nearRaw != "" {
		f, rest, err := splitField(nearRaw, "near")
		if err != nil {
			return nil, err
		}
		n, err := parseNums(rest, 3, "near")
		if err != nil {
			return nil, err
		}
		if err := checkNear(n[0], n[1], n[2]); err != nil {
			return nil, err
		}
		if err := geoField(col, f, superuser, "near"); err != nil {
			return nil, err
		}
		s.field, s.near, s.km = f, &Point{n[0], n[1]}, n[2]
	}
	if bboxRaw != "" {
		f, rest, err := splitField(bboxRaw, "bbox")
		if err != nil {
			return nil, err
		}
		n, err := parseNums(rest, 4, "bbox")
		if err != nil {
			return nil, err
		}
		b := BBox{n[0], n[1], n[2], n[3]}
		if err := checkBBox(b); err != nil {
			return nil, err
		}
		if err := geoField(col, f, superuser, "bbox"); err != nil {
			return nil, err
		}
		s.bboxField, s.bbox = f, &b
	}
	if sv := q.Get("sort"); sv != "" {
		if s.near != nil {
			return nil, bad("sort", "results are ordered by distance, sort is not supported together with near.")
		}
		s.sort = sv
	}
	s.filter = q.Get("filter")
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, bad("page", "must be a positive integer.")
		}
		s.page = n
	}
	if v := q.Get("perPage"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxPerPage {
			return nil, bad("perPage", "must be an integer within 1..%d.", MaxPerPage)
		}
		s.perPage = n
	}
	if v := q.Get("skipTotal"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, bad("skipTotal", "must be a boolean.")
		}
		s.skipTotal = b
	}
	return s, nil
}

func nearHandler(e *core.RequestEvent) error {
	collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || collection == nil {
		return e.NotFoundError("Missing collection context.", err)
	}
	if err := apis.CheckRecordsRateLimit(e, collection); err != nil {
		return err
	}
	info, err := e.RequestInfo()
	if err != nil {
		return e.BadRequestError("", err)
	}
	if collection.ListRule == nil && !info.HasSuperuserAuth() {
		return e.ForbiddenError("Only superusers can perform this action.", nil)
	}
	if err := apis.CheckSuperuserOnlyQueryFields(info); err != nil {
		return err
	}

	s, err := parseQuery(e.Request.URL.Query(), collection, info.HasSuperuserAuth())
	if err != nil {
		var pe *paramError
		if errors.As(err, &pe) {
			return paramErr(e, pe)
		}
		return e.BadRequestError("", err)
	}
	s.info = info
	s.enforce = !info.HasSuperuserAuth()

	res, err := s.run(e.App)
	if err != nil {
		var pe *paramError
		if errors.As(err, &pe) {
			return paramErr(e, pe)
		}
		return e.BadRequestError("Failed to load the records.", err)
	}

	records := make([]*core.Record, len(res.Hits))
	dist := make(map[string]float64, len(res.Hits))
	for i, h := range res.Hits {
		records[i] = h.Record
		if !math.IsNaN(h.DistanceKm) {
			dist[h.Record.Id] = h.DistanceKm
		}
	}
	result := &search.Result{Page: res.Page, PerPage: res.PerPage, TotalItems: res.TotalItems, TotalPages: res.TotalPages, Items: records}

	event := new(core.RecordsListRequestEvent)
	event.RequestEvent = e
	event.Collection = collection
	event.Records = records
	event.Result = result

	return e.App.OnRecordsListRequest().Trigger(event, func(ev *core.RecordsListRequestEvent) error {
		if err := apis.EnrichRecords(ev.RequestEvent, ev.Records); err != nil {
			return e.InternalServerError("Failed to enrich records", err)
		}
		items := make([]item, len(ev.Records))
		for i, r := range ev.Records {
			d, ok := dist[r.Id]
			items[i] = item{rec: r, dist: d, has: ok}
		}
		out := *ev.Result
		out.Items = items
		return ev.JSON(http.StatusOK, &out)
	})
}
