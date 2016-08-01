package rpc

import (
	"github.com/getlantern/tibsdb"
	"google.golang.org/grpc"
	"net"
)

type Server interface {
	Query(*Query, grpc.ServerStream) error
}

func Serve(db *tibsdb.DB, l net.Listener) error {
	gs := grpc.NewServer(grpc.CustomCodec(msgpackCodec))
	gs.RegisterService(&serviceDesc, &server{db})
	return gs.Serve(l)
}

type server struct {
	db *tibsdb.DB
}

func (s *server) Query(query *Query, stream grpc.ServerStream) error {
	q, err := s.db.SQLQuery(query.SQL)
	if err != nil {
		return err
	}
	result, err := q.Run()
	if err != nil {
		return err
	}

	entries := result.Entries
	result.Entries = nil

	// Send header
	err = stream.SendMsg(result)
	if err != nil {
		return err
	}

	// Write each entry
	for _, entry := range entries {
		row := &Row{
			Dims:   make([]interface{}, 0, len(result.GroupBy)),
			Fields: make([][]float64, 0, len(result.Fields)),
		}
		for _, dim := range result.GroupBy {
			row.Dims = append(row.Dims, entry.Dims[dim])
		}
		for i, field := range result.Fields {
			vals := entry.Fields[i]
			values := make([]float64, 0, result.NumPeriods)
			for j := 0; j < result.NumPeriods; j++ {
				val, _ := vals.ValueAt(j, field)
				values = append(values, val)
			}
			row.Fields = append(row.Fields, values)
		}
		err = stream.SendMsg(entry)
		if err != nil {
			return err
		}
	}

	return nil
}
