package guard

import (
	"reflect"
	"testing"
)

func TestGlobMatching(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/src/test/**", "app/src/test/java/FooTest.kt", true},
		{"**/src/test/**", "src/test/Foo.kt", true},
		{"**/src/test/**", "app/src/main/Foo.kt", false},
		{"**/src/androidTest/**", "app/src/androidTest/Screen.kt", true},
		{"**/*Test.kt", "app/src/main/java/com/x/FooTest.kt", true},
		{"**/*Test.kt", "FooTest.kt", true},
		{"**/*Test.kt", "app/FooTest.java", false},
		{"**/*Test.kt", "app/src/TestFoo.kt", false},
		{"tests/**", "tests/app_test.txt", true},
		{"tests/**", "src/tests/x", false},
		{"**/*Test.kt", `app\src\FooTest.kt`, true}, // backslash paths
	}
	for _, c := range cases {
		g, err := CompileGlob(c.pattern)
		if err != nil {
			t.Fatalf("compile %q: %v", c.pattern, err)
		}
		if got := g.Match(c.path); got != c.want {
			t.Errorf("glob %q match %q = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestCheckFixDiff(t *testing.T) {
	globs, err := CompileGlobs([]string{"**/src/test/**", "**/*Test.kt"})
	if err != nil {
		t.Fatal(err)
	}
	changed := []string{"app/src/main/Foo.kt", "app/src/test/FooTest.kt"}

	if v := CheckFixDiff(changed, "code_bug", true, globs); !reflect.DeepEqual(v, []string{"app/src/test/FooTest.kt"}) {
		t.Errorf("code_bug should flag test files, got %v", v)
	}
	if v := CheckFixDiff(changed, "test_bug", true, globs); v != nil {
		t.Errorf("test_bug must allow test edits, got %v", v)
	}
	if v := CheckFixDiff(changed, "code_bug", false, globs); v != nil {
		t.Errorf("protection off must allow, got %v", v)
	}
	if v := CheckFixDiff(changed, "", true, globs); v != nil {
		t.Errorf("no classification (review-driven fix) must allow, got %v", v)
	}
}
