package apt

import "strings"

// debianVersionCompare implements Debian's epoch/upstream/revision ordering.
// It is intentionally local so package selection does not depend on the host
// distribution's dpkg binary being present in the container.
func debianVersionCompare(left, right string) int {
	leftEpoch, leftUpstream, leftRevision := splitDebianVersion(left)
	rightEpoch, rightUpstream, rightRevision := splitDebianVersion(right)
	if leftEpoch != rightEpoch {
		if leftEpoch < rightEpoch {
			return -1
		}
		return 1
	}
	if compared := compareDebianPart(leftUpstream, rightUpstream); compared != 0 {
		return compared
	}
	return compareDebianPart(leftRevision, rightRevision)
}

func splitDebianVersion(version string) (epoch int, upstream, revision string) {
	if colon := strings.IndexByte(version, ':'); colon >= 0 {
		for _, digit := range version[:colon] {
			if digit < '0' || digit > '9' {
				break
			}
			epoch = epoch*10 + int(digit-'0')
		}
		version = version[colon+1:]
	}
	if dash := strings.LastIndexByte(version, '-'); dash >= 0 {
		return epoch, version[:dash], version[dash+1:]
	}
	return epoch, version, "0"
}

func compareDebianPart(left, right string) int {
	i, j := 0, 0
	for i < len(left) || j < len(right) {
		for (i < len(left) && !isDigit(left[i])) || (j < len(right) && !isDigit(right[j])) {
			leftOrder, rightOrder := 0, 0
			if i < len(left) && !isDigit(left[i]) {
				leftOrder = debianOrder(left[i])
				i++
			}
			if j < len(right) && !isDigit(right[j]) {
				rightOrder = debianOrder(right[j])
				j++
			}
			if leftOrder != rightOrder {
				if leftOrder < rightOrder {
					return -1
				}
				return 1
			}
		}
		for i < len(left) && left[i] == '0' {
			i++
		}
		for j < len(right) && right[j] == '0' {
			j++
		}
		leftDigits, rightDigits := i, j
		for i < len(left) && isDigit(left[i]) {
			i++
		}
		for j < len(right) && isDigit(right[j]) {
			j++
		}
		if i-leftDigits != j-rightDigits {
			if i-leftDigits < j-rightDigits {
				return -1
			}
			return 1
		}
		if segment := strings.Compare(left[leftDigits:i], right[rightDigits:j]); segment != 0 {
			return segment
		}
	}
	return 0
}

func isDigit(value byte) bool { return value >= '0' && value <= '9' }

func debianOrder(value byte) int {
	if value == '~' {
		return -1
	}
	if (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z') {
		return int(value)
	}
	return int(value) + 256
}
