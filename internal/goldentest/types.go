package goldentest

import (
	"net/http"
	"time"
)

const SchemaVersion = 1

type BodyKindRequest string

const (
	BodyKindReqNone  BodyKindRequest = "none"
	BodyKindReqBytes BodyKindRequest = "bytes"
	BodyKindReqJSON  BodyKindRequest = "json"
	BodyKindReqXML   BodyKindRequest = "xml"
	BodyKindReqForm  BodyKindRequest = "form"
)

type BodyKindResponse string

const (
	BodyKindRespNone        BodyKindResponse = "none"
	BodyKindRespBytes       BodyKindResponse = "bytes"
	BodyKindRespJSON        BodyKindResponse = "json"
	BodyKindRespXML         BodyKindResponse = "xml"
	BodyKindRespHTML        BodyKindResponse = "html"
	BodyKindRespMultistatus BodyKindResponse = "dav-multistatus"
)

type Case struct {
	ID            string
	SchemaVersion int
	CapturedAt    time.Time
	CapturedFrom  CaptureProvenance
	Synthetic     bool
	Replayable    bool
	Tags          []string
	PrereqCases   []string
	Request       Request
	Response      Response

	Dir         string
	RequestRaw  []byte
	ResponseRaw []byte
}

type CaptureProvenance struct {
	ServerVersion string
	Client        string
	Notes         string
}

type Request struct {
	Method        string
	Path          string
	HeadersStrict []string
	BodyKind      BodyKindRequest
}

type Response struct {
	Status        int
	HeadersStrict []string
	BodyKind      BodyKindResponse
	Normalize     []NormalizeRule
	Assertions    []Assertion
}

type NormalizeRule struct {
	DropHeaders              []string
	ReplaceHeader            *ReplaceHeaderRule
	JSONPointerRedact        []string
	XMLXPathRedact           []string
	DAVMultistatusSortByHref bool
}

type ReplaceHeaderRule struct {
	Name string
	With string
}

type AssertionKind string

const (
	AssertStatusEq                AssertionKind = "status_eq"
	AssertHeadersSubset           AssertionKind = "headers_subset"
	AssertBodyEqualAfterNormalize AssertionKind = "body_equal_after_normalize"
)

type Assertion struct {
	Kind AssertionKind
}

type ParsedRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
}

type ParsedResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}
