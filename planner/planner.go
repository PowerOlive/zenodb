// Package planner provides functionality for planning the execution of queries.
package planner

import (
	"context"
	"errors"
	"github.com/getlantern/bytemap"
	"github.com/getlantern/golog"
	"github.com/getlantern/zenodb/core"
	"github.com/getlantern/zenodb/sql"
	"sync/atomic"
	"time"
)

var (
	ErrNestedFromSubquery = errors.New("nested FROM subqueries not supported")

	log = golog.LoggerFor("planner")
)

type Opts struct {
	GetTable      func(table string) core.RowSource
	Now           func(table string) time.Time
	FieldSource   sql.FieldSource
	Distributed   bool
	PartitionKeys []string
}

func Plan(sqlString string, opts *Opts) (core.FlatRowSource, error) {
	query, err := sql.Parse(sqlString, opts.FieldSource)
	if err != nil {
		return nil, err
	}

	var source core.RowSource
	if query.FromSubQuery != nil {
		subSource, err := Plan(query.FromSubQuery.SQL, opts)
		if err != nil {
			return nil, err
		}
		unflatten := core.Unflatten(query.FromSubQuery.Fields...)
		unflatten.Connect(subSource)
		source = unflatten
	} else {
		source = opts.GetTable(query.From)
	}

	now := opts.Now(query.From)
	if query.AsOfOffset != 0 {
		query.AsOf = now.Add(query.AsOfOffset)
	}
	if query.UntilOffset != 0 {
		query.Until = now.Add(query.UntilOffset)
	}

	asOfChanged := !query.AsOf.IsZero() && query.AsOf.UnixNano() != source.GetAsOf().UnixNano()
	untilChanged := !query.Until.IsZero() && query.Until.UnixNano() != source.GetUntil().UnixNano()
	resolutionChanged := query.Resolution != 0 && query.Resolution != source.GetResolution()

	if query.Where != nil {
		runSubQueries, subQueryPlanErr := planSubQueries(query, opts)
		if subQueryPlanErr != nil {
			return nil, subQueryPlanErr
		}

		hasRunSubqueries := int32(0)
		filter := &core.Filter{
			Include: func(ctx context.Context, key bytemap.ByteMap, vals core.Vals) (bool, error) {
				if atomic.CompareAndSwapInt32(&hasRunSubqueries, 0, 1) {
					// TODO: timeout error should be okay, just means that our results are incomplete
					// Or, should we just handle timeouts as fatal across the board?
					err := runSubQueries(ctx)
					if err != nil && err != core.ErrDeadlineExceeded {
						return false, err
					}
				}
				result := query.Where.Eval(key)
				return result != nil && result.(bool), nil
			},
			Label: query.WhereSQL,
		}
		filter.Connect(source)
		source = filter
	}

	if asOfChanged || untilChanged || resolutionChanged || !query.GroupByAll || query.HasSpecificFields {
		// Need to do a group by
		group := &core.Group{
			By:         query.GroupBy,
			Fields:     query.Fields,
			Resolution: query.Resolution,
			AsOf:       query.AsOf,
			Until:      query.Until,
		}
		group.Connect(source)
		source = group
	}

	flatten := core.Flatten()
	flatten.Connect(source)
	var flat core.FlatRowSource = flatten

	if len(query.OrderBy) > 0 {
		sort := core.Sort(query.OrderBy...)
		sort.Connect(flat)
		flat = sort
	}

	if query.Offset > 0 {
		offset := core.Offset(query.Offset)
		offset.Connect(flat)
		flat = offset
	}

	if query.Limit > 0 {
		limit := core.Limit(query.Limit)
		limit.Connect(flat)
		flat = limit
	}

	return flat, nil
}
