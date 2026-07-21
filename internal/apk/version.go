package apk

import "strings"

// apkVersionCompare covers the normal Alpine version/revision form used in an
// APKINDEX (for example 3.20.2-r1). It compares numeric runs numerically and
// text runs lexically, then compares the -r revision numerically.
func apkVersionCompare(left, right string) int {
	leftBase, leftRevision := splitVersion(left)
	rightBase, rightRevision := splitVersion(right)
	if compared := compareVersionBase(leftBase, rightBase); compared != 0 {
		return compared
	}
	if leftRevision < rightRevision {
		return -1
	}
	if leftRevision > rightRevision {
		return 1
	}
	return 0
}

func splitVersion(version string) (string, int) {
	if marker := strings.LastIndex(version, "-r"); marker >= 0 {
		revision := 0
		valid := true
		for _, digit := range version[marker+2:] {
			if digit < '0' || digit > '9' {
				valid = false
				break
			}
			revision = revision*10 + int(digit-'0')
		}
		if valid {
			return version[:marker], revision
		}
	}
	return version, 0
}

func compareVersionBase(left, right string) int {
	i, j := 0, 0
	for i < len(left) || j < len(right) {
		leftDigit := i < len(left) && isDigit(left[i])
		rightDigit := j < len(right) && isDigit(right[j])
		if leftDigit && rightDigit {
			for i < len(left) && left[i] == '0' {
				i++
			}
			for j < len(right) && right[j] == '0' {
				j++
			}
			leftStart, rightStart := i, j
			for i < len(left) && isDigit(left[i]) {
				i++
			}
			for j < len(right) && isDigit(right[j]) {
				j++
			}
			if i-leftStart != j-rightStart {
				if i-leftStart < j-rightStart {
					return -1
				}
				return 1
			}
			if compared := strings.Compare(left[leftStart:i], right[rightStart:j]); compared != 0 {
				return compared
			}
			continue
		}
		leftValue, rightValue := byte(0), byte(0)
		if i < len(left) {
			leftValue = left[i]
			i++
		}
		if j < len(right) {
			rightValue = right[j]
			j++
		}
		if leftValue != rightValue {
			if leftValue < rightValue {
				return -1
			}
			return 1
		}
	}
	return 0
}

func isDigit(value byte) bool { return value >= '0' && value <= '9' }
