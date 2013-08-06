package search

import (
	"fmt"
	"github.com/balzaczyy/golucene/index"
)

type Query interface {
	CreateWeight(ss IndexSearcher) Weight
	Rewrite(r index.IndexReader) Query
}

type AbstractQuery struct {
	boost float32
}

func NewAbstractQuery() *AbstractQuery {
	return &AbstractQuery{1.0}
}

func (q *AbstractQuery) CreateWeight(ss IndexSearcher) Weight {
	panic(fmt.Sprintf("Query %v does not implement createWeight", q))
}

func (q *AbstractQuery) Rewrite(r index.IndexReader) Query {
	return q
}
