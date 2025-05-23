package math_test

import (
	"context"
	"testing"

	"github.com/MontFerret/ferret/pkg/runtime/values"
	"github.com/MontFerret/ferret/pkg/stdlib/math"

	. "github.com/smartystreets/goconvey/convey"
)

func TestPercentile(t *testing.T) {
	Convey("Should return nth percentile value", t, func() {
		out, err := math.Percentile(
			context.Background(),
			values.NewArrayWith(
				values.NewInt(1),
				values.NewInt(2),
				values.NewInt(3),
				values.NewInt(4),
			),
			values.NewInt(50),
		)

		So(err, ShouldBeNil)
		So(out.Unwrap(), ShouldEqual, 2)
	})
}
