package repositories

import "testing"

func TestParseSearchQuery(t *testing.T) {
	cases := map[string]SearchQuery{
		"@Ayan_01":          {Username: "Ayan_01"},
		" @ayan ":           {Username: "ayan"},
		"https://t.me/ayan": {Username: "ayan"},
		"t.me/ayan/":        {Username: "ayan"},
		"123456789":         {ID: 123456789},
		"Аян Серик":         {Name: "Аян Серик"},
		"-5":                {Name: "-5"},
		"100%_name":         {Name: "100%_name"},
	}
	for in, want := range cases {
		if got := ParseSearchQuery(in); got != want {
			t.Errorf("%q → %+v, want %+v", in, got, want)
		}
	}
	if likeEscape(`a%b_c\`) != `a\%b\_c\\` {
		t.Fatal(likeEscape(`a%b_c\`))
	}
}

func TestUserBriefDisplayName(t *testing.T) {
	if (UserBrief{FirstName: "Аян", LastName: "С"}).DisplayName() != "Аян С" ||
		(UserBrief{Username: "x"}).DisplayName() != "@x" ||
		(UserBrief{TelegramID: 7}).DisplayName() != "7" {
		t.Fatal("display name")
	}
}
