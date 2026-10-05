package kafka

import (
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
)

func plainMech(user, pass string) sasl.Mechanism {
	return plain.Auth{User: user, Pass: pass}.AsMechanism()
}
