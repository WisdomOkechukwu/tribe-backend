package money

import (
	"math/big"
	"testing"
)

// TestParseDecimal turns what people type into smallest units.
func TestParseDecimal(test *testing.T) {
	cases := []struct {
		text          string
		decimalPlaces int
		wantUnits     string
		wantError     bool
	}{
		{"12.50", 2, "1250", false},
		{"12.5", 2, "1250", false},
		{"12", 2, "1200", false},
		{"12.500", 2, "1250", false}, // extra zeros are fine
		{"0.00000001", 8, "1", false},
		{"12.345", 2, "", true}, // more decimals than the asset has
		{"abc", 2, "", true},
		{"", 2, "", true},
		{"1e5", 2, "", true},
	}
	for _, testCase := range cases {
		amount, err := ParseDecimal(testCase.text, testCase.decimalPlaces)
		if testCase.wantError {
			if err == nil {
				test.Errorf("ParseDecimal(%q, %d) = %s, want an error", testCase.text, testCase.decimalPlaces, amount.UnitsString())
			}
			continue
		}
		if err != nil {
			test.Errorf("ParseDecimal(%q, %d): %v", testCase.text, testCase.decimalPlaces, err)
			continue
		}
		if amount.UnitsString() != testCase.wantUnits {
			test.Errorf("ParseDecimal(%q, %d) = %s, want %s", testCase.text, testCase.decimalPlaces, amount.UnitsString(), testCase.wantUnits)
		}
	}
}

// TestDecimalRoundTrip formats units back into what people read.
func TestDecimalRoundTrip(test *testing.T) {
	cases := map[string]struct {
		units         int64
		decimalPlaces int
	}{
		"12.50":      {1250, 2},
		"0.05":       {5, 2},
		"-3.10":      {-310, 2},
		"0.00000001": {1, 8},
		"7":          {7, 0},
	}
	for want, testCase := range cases {
		if got := FromUnits(testCase.units).Decimal(testCase.decimalPlaces); got != want {
			test.Errorf("FromUnits(%d).Decimal(%d) = %s, want %s", testCase.units, testCase.decimalPlaces, got, want)
		}
	}
}

// TestParseUnitsAcceptsDatabaseSums: MySQL returns SUM() as "1250.000".
func TestParseUnitsAcceptsDatabaseSums(test *testing.T) {
	amount, err := ParseUnits("1250.000")
	if err != nil || amount.UnitsString() != "1250" {
		test.Fatalf("ParseUnits(1250.000) = %s, %v", amount.UnitsString(), err)
	}
	if _, err := ParseUnits("12.5"); err == nil {
		test.Fatal("ParseUnits accepted a real fraction")
	}
}

// TestRounding: fees round UP, payouts round DOWN, display rounds half up.
func TestRounding(test *testing.T) {
	twoThirds := big.NewRat(2, 3) // 0.6666...
	if got := FromRational(twoThirds, 2, RoundDown).UnitsString(); got != "66" {
		test.Errorf("RoundDown = %s, want 66", got)
	}
	if got := FromRational(twoThirds, 2, RoundUp).UnitsString(); got != "67" {
		test.Errorf("RoundUp = %s, want 67", got)
	}
	half := big.NewRat(1, 200) // 0.005
	if got := FromRational(half, 2, RoundHalfUp).UnitsString(); got != "1" {
		test.Errorf("RoundHalfUp(0.005) = %s, want 1", got)
	}
	exact := big.NewRat(3, 2)
	for _, mode := range []RoundingMode{RoundDown, RoundUp, RoundHalfUp} {
		if got := FromRational(exact, 2, mode).UnitsString(); got != "150" {
			test.Errorf("exact 1.5 with mode %d = %s, want 150", mode, got)
		}
	}
}

// TestArithmetic covers add, subtract and comparisons.
func TestArithmetic(test *testing.T) {
	balance := FromUnits(1000)
	spent := FromUnits(1250)
	after := balance.Subtract(spent)
	if !after.IsNegative() || after.UnitsString() != "-250" {
		test.Fatalf("1000 - 1250 = %s", after.UnitsString())
	}
	if after.Negate().Compare(FromUnits(250)) != 0 {
		test.Fatal("Negate(-250) != 250")
	}
	if !Zero().IsZero() || Zero().IsPositive() {
		test.Fatal("Zero is not zero")
	}
}

// TestDisplayGroupsThousandsAndTrimsCoinZeros checks the text customers
// read in their inbox: commas between thousands, cents kept on money, and
// no trailing zeros on a coin amount.
func TestDisplayGroupsThousandsAndTrimsCoinZeros(test *testing.T) {
	cases := []struct {
		units         string
		decimalPlaces int
		want          string
	}{
		{"25000000", 2, "250,000.00"},
		{"66000", 2, "660.00"},
		{"300000000", 6, "300"},
		{"500000", 8, "0.005"},
		{"123456789012", 8, "1,234.56789012"},
		{"-125000", 2, "-1,250.00"},
		{"1000", 0, "1,000"},
		{"5", 2, "0.05"},
	}
	for _, testCase := range cases {
		amount, err := ParseUnits(testCase.units)
		if err != nil {
			test.Fatalf("ParseUnits(%s): %v", testCase.units, err)
		}
		if got := amount.Display(testCase.decimalPlaces); got != testCase.want {
			test.Errorf("Display(%s units, %d places) = %s, want %s", testCase.units, testCase.decimalPlaces, got, testCase.want)
		}
	}
}
