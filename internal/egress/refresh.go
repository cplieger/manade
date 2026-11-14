package egress

import (
	"fmt"
	"strings"
)

// Refresh answers the payload that adds ls to a loaded rule set's learned
// sets, renewing the timeout of any already there: for NFTables an `nft -f`
// transaction, for IPTables an `ipset restore -exist` payload. Each address
// must have passed Learnable and have time left; IPv6 ones need the IPv6
// set, which only a host with ipv6 has.
func Refresh(b Backend, ls []Learned) string {
	var w strings.Builder
	for _, l := range ls {
		a := l.Addr.Unmap()
		name := setGitHub4
		if a.Is6() {
			name = setGitHub6
		}
		if b == IPTables {
			fmt.Fprintf(&w, "add %s %s timeout %d\n", name, a, seconds(l.Left))
			continue
		}
		// An nft add keeps an existing element's timeout and a delete of a
		// missing one fails the transaction: add, delete, add renews either way.
		el := fmt.Sprintf("inet %s %s { %s", nftTable, name, a)
		fmt.Fprintf(&w, "add element %s timeout %ds }\ndelete element %s }\nadd element %s timeout %ds }\n",
			el, seconds(l.Left), el, el, seconds(l.Left))
	}
	return w.String()
}
