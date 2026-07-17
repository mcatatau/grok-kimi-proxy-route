package main

import (
	"fmt"

	"grok-desktop/internal/store"
)

func main() {
	st, err := store.Open("")
	if err != nil {
		panic(err)
	}
	defer st.Close()
	accs := st.ListAccountsForProvider(store.ProviderKimiWork)
	for _, a := range accs {
		if err := st.ClearAuthState(a.ID); err != nil {
			fmt.Println("clear", a.ID, err)
		} else {
			fmt.Println("cleared", a.ID)
		}
	}
	accs = st.ListAccountsForProvider(store.ProviderKimiWork)
	for _, a := range accs {
		fmt.Printf("id=%s usable=%v denied=%v label=%q\n", a.ID, a.Usable(), a.AuthDenied(), a.Label)
	}
}
