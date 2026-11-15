// Package money keeps every amount as a whole number of the asset's
// smallest unit (kobo for NGN, cents for USD, satoshi for BTC ...).
//
// Why? Computers cannot store 0.1 exactly as a float, so 0.1 + 0.2 is not
// 0.3. Money must never drift, so we never use float64 for money. Instead we
// use big.Int (a whole number of any size) and only turn it into a
// human-friendly decimal string ("12.50") when we show it.
package money

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// ErrInvalidAmount is returned when a text amount cannot be understood.
var ErrInvalidAmount = errors.New("invalid amount")

// ErrTooManyDecimals is returned when "1.234" is sent for an asset that only has 2 decimals.
var ErrTooManyDecimals = errors.New("too many decimal places")

// maximumDigits is the largest number of digits our DECIMAL(38,0) columns can hold.
const maximumDigits = 38

// Amount is a whole number of smallest units, for example 1250 kobo = ₦12.50.
type Amount struct {
	units *big.Int
}

// Zero returns an amount of 0.
func Zero() Amount {
	return Amount{units: big.NewInt(0)}
}

// FromUnits builds an amount from a whole number of smallest units.
func FromUnits(units int64) Amount {
	return Amount{units: big.NewInt(units)}
}

// FromBigInt builds an amount from a big.Int (the value is copied).
func FromBigInt(units *big.Int) Amount {
	return Amount{units: new(big.Int).Set(units)}
}

// ParseUnits reads a whole-number string such as "1250" (this is how the
// database returns DECIMAL(38,0) columns).
func ParseUnits(text string) (Amount, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Zero(), nil
	}
	// The database may return "1250.000" for SUM() results, so drop a zero fraction.
	if dotIndex := strings.Index(text, "."); dotIndex >= 0 {
		if strings.Trim(text[dotIndex+1:], "0") != "" {
			return Zero(), ErrInvalidAmount
		}
		text = text[:dotIndex]
	}
	units, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return Zero(), ErrInvalidAmount
	}
	return Amount{units: units}, nil
}

// ParseDecimal reads what a person types, for example "12.50", and turns it
// into smallest units using the asset's number of decimal places.
// ParseDecimal("12.50", 2) gives 1250.
func ParseDecimal(text string, decimalPlaces int) (Amount, error) {
	text = strings.TrimSpace(strings.ReplaceAll(text, ",", ""))
	if text == "" {
		return Zero(), ErrInvalidAmount
	}
	isNegative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")

	wholePart, fractionPart, _ := strings.Cut(text, ".")
	if wholePart == "" {
		wholePart = "0"
	}
	if len(fractionPart) > decimalPlaces {
		// Allow extra zeros ("12.500" for a 2-decimal asset) but nothing else.
		if strings.Trim(fractionPart[decimalPlaces:], "0") != "" {
			return Zero(), ErrTooManyDecimals
		}
		fractionPart = fractionPart[:decimalPlaces]
	}
	fractionPart += strings.Repeat("0", decimalPlaces-len(fractionPart))

	for _, character := range wholePart + fractionPart {
		if character < '0' || character > '9' {
			return Zero(), ErrInvalidAmount
		}
	}
	units, ok := new(big.Int).SetString(wholePart+fractionPart, 10)
	if !ok {
		return Zero(), ErrInvalidAmount
	}
	if len(units.String()) > maximumDigits {
		return Zero(), ErrInvalidAmount
	}
	if isNegative {
		units.Neg(units)
	}
	return Amount{units: units}, nil
}

// BigInt returns a copy of the smallest units as a big.Int.
func (amount Amount) BigInt() *big.Int {
	if amount.units == nil {
		return big.NewInt(0)
	}
	return new(big.Int).Set(amount.units)
}

// UnitsString returns the smallest units as text, for example "1250". This
// is what we save into DECIMAL(38,0) columns.
func (amount Amount) UnitsString() string {
	return amount.BigInt().String()
}

// Decimal returns a human-friendly string such as "12.50".
func (amount Amount) Decimal(decimalPlaces int) string {
	units := amount.BigInt()
	sign := ""
	if units.Sign() < 0 {
		sign = "-"
		units.Neg(units)
	}
	digits := units.String()
	if decimalPlaces == 0 {
		return sign + digits
	}
	if len(digits) <= decimalPlaces {
		digits = strings.Repeat("0", decimalPlaces-len(digits)+1) + digits
	}
	splitAt := len(digits) - decimalPlaces
	return fmt.Sprintf("%s%s.%s", sign, digits[:splitAt], digits[splitAt:])
}

// Display returns the amount the way a person reads it in a message:
// thousands grouped with commas, and no pointless zeros on the end of a coin
// amount. Display(2) of 25000000 gives "250,000.00"; Display(8) of 500000
// gives "0.005". Money with two decimals or fewer keeps them all ("1,250.00").
// Use it for inbox and notification text only: JSON amounts stay Decimal.
func (amount Amount) Display(decimalPlaces int) string {
	plain := amount.Decimal(decimalPlaces)
	sign := ""
	if strings.HasPrefix(plain, "-") {
		sign, plain = "-", plain[1:]
	}
	wholePart, fractionPart, _ := strings.Cut(plain, ".")
	if decimalPlaces > 2 {
		fractionPart = strings.TrimRight(fractionPart, "0")
	}
	// Walk the whole part from the left, adding a comma before every group of three digits.
	var grouped strings.Builder
	for position, digit := range wholePart {
		if position > 0 && (len(wholePart)-position)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	if fractionPart == "" {
		return sign + grouped.String()
	}
	return sign + grouped.String() + "." + fractionPart
}

// Add returns amount + other.
func (amount Amount) Add(other Amount) Amount {
	return Amount{units: new(big.Int).Add(amount.BigInt(), other.BigInt())}
}

// Subtract returns amount - other.
func (amount Amount) Subtract(other Amount) Amount {
	return Amount{units: new(big.Int).Sub(amount.BigInt(), other.BigInt())}
}

// Negate returns -amount.
func (amount Amount) Negate() Amount {
	return Amount{units: new(big.Int).Neg(amount.BigInt())}
}

// IsPositive is true when the amount is more than zero.
func (amount Amount) IsPositive() bool { return amount.BigInt().Sign() > 0 }

// IsNegative is true when the amount is less than zero.
func (amount Amount) IsNegative() bool { return amount.BigInt().Sign() < 0 }

// IsZero is true when the amount is exactly zero.
func (amount Amount) IsZero() bool { return amount.BigInt().Sign() == 0 }

// Compare returns -1 if amount < other, 0 if equal and +1 if amount > other.
func (amount Amount) Compare(other Amount) int {
	return amount.BigInt().Cmp(other.BigInt())
}

// ToRational returns the amount in whole asset units as an exact fraction
// (1250 kobo with 2 decimals becomes 12.5). Used for rate maths.
func (amount Amount) ToRational(decimalPlaces int) *big.Rat {
	return new(big.Rat).SetFrac(amount.BigInt(), PowerOfTen(decimalPlaces))
}

// RoundingMode tells FromRational how to turn a fraction into whole units.
type RoundingMode int

const (
	// RoundDown always rounds towards zero. Used for what we pay to users,
	// so we never pay out a fraction of a kobo we do not have.
	RoundDown RoundingMode = iota
	// RoundUp always rounds away from zero. Used for fees we charge.
	RoundUp
	// RoundHalfUp rounds to the nearest unit (0.5 goes up). Used for display values.
	RoundHalfUp
)

// FromRational turns an exact fraction of whole asset units (for example
// 12.505 dollars) into smallest units, using the rounding mode given.
func FromRational(value *big.Rat, decimalPlaces int, mode RoundingMode) Amount {
	scaled := new(big.Rat).Mul(value, new(big.Rat).SetInt(PowerOfTen(decimalPlaces)))
	numerator := scaled.Num()
	denominator := scaled.Denom()

	quotient, remainder := new(big.Int).QuoRem(numerator, denominator, new(big.Int))
	if remainder.Sign() == 0 {
		return Amount{units: quotient}
	}
	switch mode {
	case RoundUp:
		if numerator.Sign() > 0 {
			quotient.Add(quotient, big.NewInt(1))
		} else {
			quotient.Sub(quotient, big.NewInt(1))
		}
	case RoundHalfUp:
		doubleRemainder := new(big.Int).Mul(new(big.Int).Abs(remainder), big.NewInt(2))
		if doubleRemainder.Cmp(denominator) >= 0 {
			if numerator.Sign() > 0 {
				quotient.Add(quotient, big.NewInt(1))
			} else {
				quotient.Sub(quotient, big.NewInt(1))
			}
		}
	}
	return Amount{units: quotient}
}

// PowerOfTen returns 10^exponent as a big.Int.
func PowerOfTen(exponent int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
}

// ParseRate reads a decimal rate string such as "1540.25" into an exact fraction.
func ParseRate(text string) (*big.Rat, error) {
	rate, ok := new(big.Rat).SetString(strings.TrimSpace(text))
	if !ok {
		return nil, fmt.Errorf("invalid rate %q", text)
	}
	return rate, nil
}

// FormatRate turns an exact fraction into a decimal string with a fixed
// number of decimal places (rates are stored with 18).
func FormatRate(rate *big.Rat, decimalPlaces int) string {
	return rate.FloatString(decimalPlaces)
}
