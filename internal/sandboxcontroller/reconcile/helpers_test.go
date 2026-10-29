package reconcile_test

import (
	"fmt"

	"pdd/services/internal/sandboxcontroller/source"
)

func sourceErr() error { return fmt.Errorf("%w: gateway answered 404", source.ErrUnavailable) }
