// File: gateway/pkg/sip/sip.go
package sip

import (
	sipgo "github.com/emiago/sipgo/sip"
)

// -------------------------------------------------------------------
// Re-export sipgo interface + parse method
// -------------------------------------------------------------------

// Message is re-exported from sipgo, so you can refer to sip.Message in your code
type Message = sipgo.Message

// ParseMessage is a convenience function to parse raw SIP data
func ParseMessage(data []byte) (Message, error) {
	return sipgo.ParseMessage(data)
}

// Similarly, if you need to re-export Request, Response, etc. from sipgo:
type Request = sipgo.Request
type Response = sipgo.Response
type ServerTransaction = sipgo.ServerTransaction
type ClientTransaction = sipgo.ClientTransaction

var (
	ErrTransactionCanceled = sipgo.ErrTransactionCanceled
	CANCEL                 = sipgo.CANCEL
)

// Add more aliases or wrappers as needed
