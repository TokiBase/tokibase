package search_test

import "fmt"

// handCorpus is the hand written differential corpus: every operator,
// modifier, macro and function plus nasty cases. Field names refer to the
// tests/data fixtures (demo1, demo2, demo4, demo5, users).
func handCorpus() []string {
	c := []string{
		// --- every operator vs literal
		`text = 'abc'`, `text != 'abc'`, `text ~ 'abc'`, `text !~ 'abc'`,
		`number > 1`, `number >= 1`, `number < 1`, `number <= 1`,
		`select_many ?= 'optionA'`, `select_many ?!= 'optionA'`, `select_many ?~ 'opt'`, `select_many ?!~ 'opt'`,
		`rel_many.title ?> 'a'`, `rel_many.title ?>= 'a'`, `rel_many.title ?< 'a'`, `rel_many.title ?<= 'a'`,
		`rel_many.title = 'a'`, `rel_many.title != 'a'`, `rel_many.title ~ 'a'`, `rel_many.title !~ 'a'`,
		`rel_many.title > 'a'`, `rel_many.title >= 'a'`, `rel_many.title < 'a'`, `rel_many.title <= 'a'`,
		`rel_many.title ?= 'a'`, `rel_many.title ?!= 'a'`, `rel_many.title ?~ 'a'`, `rel_many.title ?!~ 'a'`,
		// --- null / bool / empty handling
		`text = null`, `text != null`, `null = text`, `null != text`, `null = null`, `null != null`,
		`text = ''`, `text != ''`, `'' = text`, `'' != ''`, `'' = ''`,
		`bool = true`, `bool = false`, `bool != true`, `true = bool`, `bool = 1`, `bool = 0`, `true = true`, `true = false`,
		`bool = TRUE`, `bool = Null`, `NULL = text`, `FALSE = bool`,
		`number = null`, `number != null`, `number = 0`, `number != 0`, `json = null`, `json != null`,
		`json = 'x'`, `json.a = 1`, `json.a.b.c = 'x'`, `json_array = 1`,
		`rel_one = ''`, `rel_one != ''`, `rel_one = null`, `rel_many = ''`, `rel_many ?= ''`, `rel_many ?= null`,
		`file_one = ''`, `file_many != ''`, `select_one = ''`, `select_many = ''`, `select_many ?= ''`,
		// --- mixed types, numbers vs strings
		`number = '1'`, `number = 1.5`, `number = -3`, `number = 1e3`, `number = 0.0001`, `number > 9007199254740993`,
		`text = 1`, `text = 1.25`, `text = '1'`, `1 = 1`, `1 = '1'`, `'a' = 'a'`, `'a' != 'b'`, `1 < 2`, `'a' ~ 'a'`, `1.5 ~ 1`,
		`number = text`, `text = number`, `text = text`, `text ~ text`, `number > number`, `bool = number`,
		// --- like escaping
		`text ~ 'a%b'`, `text ~ 'a_b'`, `text ~ 'a\\%b'`, `text ~ '%'`, `text ~ '_'`, `text ~ '\\'`, `text !~ 'a%'`, `text ~ 'a\\\\%'`,
		`text ~ email`, `text !~ email`, `email ~ 'example.com'`,
		// --- quoting, escapes, unicode
		`text = "double"`, `text = 'sin\'gle'`, `text = "dou\"ble"`, `text = 'a"b'`, `text = "a'b"`,
		`text = 'unicode: čšž 日本語 😀'`, `text = "emoji 😀"`, `text ~ 'ß'`, `text = 'tab\tnew\nline'`, `text = ''''`,
		`text = '\\'`, `text = "\\\\"`, `text = 'a--b'`, `text = 'a//b'`, `text = '/* c */'`, `text = '&& ||'`, `text = '(x)'`,
		// --- groups and joins
		`(text = 'a')`, `((text = 'a'))`, `(((text = 'a')))`, `text = 'a' && number = 1`, `text = 'a' || number = 1`,
		`text = 'a' && number = 1 || bool = true`, `text = 'a' || number = 1 && bool = true`,
		`(text = 'a' || number = 1) && bool = true`, `text = 'a' && (number = 1 || bool = true)`,
		`(text = 'a' && number = 1) || (bool = true && email != '')`, `((text = 'a' || text = 'b') && (number = 1 || number = 2)) || id = 'x'`,
		`(text = 'a') && (number = 1) && (bool = true)`, `(text = 'a') || (number = 1) || (bool = true)`,
		`text = 'a' && text = 'b' && text = 'c' && text = 'd' && text = 'e'`,
		`(text = 'a' && (number = 1 || (bool = true && (email != '' || id = 'z'))))`,
		`  text   =   'a'  &&   number=1  `, "text = 'a'\n&&\nnumber = 1", "text\t=\t'a'",
		`text = 'a' // trailing comment`, "// leading comment\ntext = 'a'", "text = 'a' /* not a comment */ ", `text = 'a' && /* x */ number = 1`,
		// --- modifiers
		`select_many:length = 2`, `select_many:length > 0`, `file_many:length = 0`, `rel_many:length >= 1`, `rel_many.rel_many:length = 1`,
		`select_many:length ?= 1`, `json_array:length = 1`, `json:length = 1`, `file_many:length != number`,
		`text:lower = 'abc'`, `text:lower ~ 'abc'`, `email:lower = 'a@b.c'`, `rel_one.title:lower = 'a'`, `rel_many.title:lower ?= 'a'`,
		`select_many:each ~ 'opt'`, `select_many:each != 'optionA'`, `select_many:each = 'optionA'`, `file_many:each = 'x'`, `rel_many:each = 'x'`,
		`@request.body.text:isset = true`, `@request.body.text:isset = false`, `@request.body.missing:isset = 1`,
		`@request.query.q:isset = true`, `@request.headers.x_token:isset = true`, `@request.auth.id:isset = true`,
		`@request.body.select_many:each ~ 'a'`, `@request.body.list:each = 'x'`, `@request.body.rel_many:length = 2`, `@request.body.text:lower = 'a'`,
		`@request.body.text:changed = true`, `@request.body.number:changed = false`, `@request.body.nested.field:isset = true`,
		`text:isset = true`, `number:isset = true`, `rel_one.title:isset = true`,
		// --- @request
		`@request.auth.id != ''`, `@request.auth.id = ''`, `@request.auth.id = null`, `@request.auth.id != null`, `@request.auth.id = id`,
		`@request.auth.collectionName = 'users'`, `@request.auth.collectionId != ''`, `@request.auth.email ~ 'example'`, `@request.auth.verified = true`,
		`@request.auth.rel.title = 'x'`, `@request.auth.rel.rel_many.title ?= 'x'`, `@request.auth.username = text`, `@request.auth.name:lower = 'x'`,
		`@request.method = 'GET'`, `@request.method != 'POST'`, `@request.context = 'default'`, `@request.context != 'oauth2'`,
		`@request.query.a = 'b'`, `@request.query.a = number`, `@request.query.a.b = 'c'`, `@request.headers.content_type ~ 'json'`,
		`@request.body.text = text`, `@request.body.text != text`, `@request.body.number > number`, `@request.body.text = ''`, `@request.body.text = null`,
		`@request.body.rel_one = rel_one`, `@request.body.rel_many ?= rel_many`, `@request.body.rel_many = rel_many`, `@request.body.rel_many.title = 'x'`,
		`@request.body.json.a = 1`, `@request.body.bool = true`, `@request.body.number = 1`, `@request.body.unknown = 1`,
		`@request.auth.id != '' && @request.body.text = 'x' || @request.query.q = 'y'`,
		`@request.body.select_many ?= 'a'`, `@request.body.select_many ?~ 'a'`, `@request.body.select_many = 'a'`,
		`@request.auth.id = @request.body.id`, `@request.method = @request.context`, `@request.query.a = @request.query.b`,
		// --- @collection
		`@collection.demo2.title = 'x'`, `@collection.demo2.id = id`, `@collection.demo2.title ?= text`, `@collection.demo2.active = true`,
		`@collection.demo2:a.title = 'x' && @collection.demo2:b.title = 'y'`, `@collection.demo2:alias.id = rel_one`, `@collection.users.id = @request.auth.id`,
		`@collection.demo1.rel_many.title ?= 'x'`, `@collection.demo1.rel_one.title = text`, `@collection.demo2.title = @collection.demo2.title`,
		`@collection.demo2:x.title = @collection.demo2:y.title`, `@collection.demo2.id = 'a' || @collection.demo2.id = 'b'`,
		`@collection.demo1.select_many:length = 2`, `@collection.demo1.text:lower = 'a'`, `@collection.missing.id = 1`,
		// --- relations / back-relations
		`rel_one.title = 'x'`, `rel_one.title != 'x'`, `rel_one.id = rel_many.id`, `rel_many.id = 'a'`, `rel_many.title ?= rel_one.title`,
		`rel_many.title = rel_many.title`, `rel_many.title ?= rel_many.title`, `rel_many.title = rel_one.title`, `rel_one.title = rel_many.title`,
		`demo1_via_rel_one.text = 'x'`, `demo1_via_rel_many.text ?= 'x'`, `demo1_via_rel_many.id != ''`, `demo1_via_rel_one.rel_many.title ?= 'x'`,
		`demo1_via_rel_one.text = demo1_via_rel_many.text`, `demo1_via_rel_one:length = 1`, `demo1_via_rel_many:length > 0`,
		`demo2.demo1_via_rel_one.text = 'x'`, `demo2_via_missing.id = 1`, `rel_one.rel_many.rel_one.title ?= 'x'`,
		`rel_many.title ~ 'x' && rel_many.title !~ 'y'`, `(rel_many.title ?= 'a' && rel_many.title ?= 'b') || rel_one.title = 'c'`,
		`self_rel_one.title = 'x'`, `self_rel_many.title ?= 'x'`, `self_rel_many.self_rel_many.title ?= 'x'`, `rel_many_cascade.id = 'x'`,
		`rel_many_no_cascade.title = rel_many_cascade.title`, `rel_one_unique.title = 'x'`, `rel_many.rel_many.rel_many:length = 1`,
		// --- time macros
		`datetime > @now`, `datetime < @now`, `datetime >= @yesterday`, `datetime <= @tomorrow`, `datetime >= @todayStart && datetime <= @todayEnd`,
		`datetime >= @monthStart && datetime <= @monthEnd`, `datetime >= @yearStart && datetime <= @yearEnd`,
		`@second > 1`, `@minute > 1`, `@hour = 1`, `@day = 1`, `@month = 1`, `@weekday = 1`, `@year = 2026`, `@now = @now`, `@now > text`,
		`@todayStart < @now`, `created > @now`, `updated >= @todayStart`, `@now ~ '2026'`, `@request.body.datetime < @now`,
		`@now = ''`, `@now != null`, `@year:lower = 1`, `@unknownmacro = 1`,
		// --- functions
		`geoDistance(1, 2, 3, 4) < 10`, `geoDistance(point.lon, point.lat, 23.32, 42.69) < 200`, `geoDistance(point.lon, point.lat, 23.32, 42.69) >= 200`,
		`geoDistance(1,2,3,4)>1`, `geoDistance(point.lon, point.lat, @request.body.lon, @request.body.lat) < 5`, `geoDistance(null, null, 1, 2) = null`,
		`geoDistance(rel_many.point.lon, rel_many.point.lat, 1, 2) < 5`, `geoDistance(1, 2, 3) < 1`, `geoDistance('a', 2, 3, 4) < 1`, `geoDistance(1, 2, 3, 4, 5) < 1`,
		`strftime('%Y', created) = '2026'`, `strftime('%Y-%m', datetime) = '2026-10'`, `strftime('%Y') = '2026'`, `strftime('%Y-%m-%d', datetime, '+1 day') > '2026-01-01'`,
		`strftime('%Y-%m-%d', datetime, 'start of month', '+1 month', '-1 day') = '2026-10-31'`, `strftime('%w', @now) = '1'`, `strftime('%Y', rel_many.created) ?= '2026'`,
		`strftime('%Y', rel_many.created) = '2026'`, `strftime(text, created) = ''`, `strftime() = 1`, `strftime('%Y', created, 1) = 1`, `strftime('%H', @request.body.datetime) = '10'`,
		`strftime('%Y', created) = strftime('%Y', updated)`, `'2026' = strftime('%Y', created)`, `strftime('%Y', created) ~ '202'`, `strftime('%Y', @now) = strftime('%Y', created) && number = 1`,
		`unknownFunc(1) = 1`, `geoDistance(geoDistance(1,2,3,4), 2, 3, 4) < 1`, `strftime('%Y', strftime('%Y', created)) = 1`,
		// --- error shapes
		``, `   `, `// only comment`, `text`, `text =`, `= 'a'`, `text = = 'a'`, `text 'a'`, `text == 'a'`, `text = 'a' &&`, `&& text = 'a'`, `text = 'a' number = 1`,
		`(text = 'a'`, `text = 'a')`, `()`, `(())`, `text = 'a' && ()`, `text = 'unterminated`, `text = "unterminated`, `text = 'a' /* unclosed`, `text = @`, `@ = 1`,
		`text = 1.2.3`, `text = 1e`, `text = --1`, `text = 'a' || || number = 1`, `text = 'a' & number = 1`, `text = 'a' | number = 1`, `a b c`, `😀 = 1`, `te-xt = 1`,
		`missing = 1`, `missing.sub = 1`, `text.sub = 1`, `rel_one.missing = 1`, `rel_one.title.sub = 1`, `id = unknown`, `password = 'x'`, `tokenKey = 'x'`, `@request.unknown = 1`,
		`@request.auth = 1`, `@request.body = 1`, `@request = 1`, `@collection = 1`, `@collection.demo2 = 1`, `text = 'a' && missing = 1`, `1 = missing`,
		`geoDistance(1, 2, 3, 4)`, `strftime('%Y')`, `'a'`, `1`, `null`, `true`,
		// --- misc realistic rules
		`id = @request.auth.id`, `@request.auth.id != "" && @request.auth.collectionName != "users"`, `@request.auth.id != '' && recordRef = @request.auth.id && collectionRef = @request.auth.collectionId`,
		`@request.auth.verified = true && (owner = @request.auth.id || @request.auth.collectionName = '_superusers')`,
		`(@request.body.total:isset = false || @request.body.total = total) && select_many:length = 3`,
		`created >= '2022-01-01 00:00:00.000Z' && created <= '2030-01-01 00:00:00.000Z'`, `id ~ 'a' || id ~ 'b' || id ~ 'c'`, `id != '' && created != ''`,
		`username:lower = @request.body.username:lower`, `email != '' && emailVisibility = true`, `verified = true && email ~ '@example.com'`,
	}

	// generated operator x operand matrix
	ops := []string{"=", "!=", ">", ">=", "<", "<=", "~", "!~", "?=", "?!=", "?>", "?>=", "?<", "?<=", "?~", "?!~"}
	lefts := []string{"text", "number", "bool", "select_many", "select_one", "rel_many.title", "rel_one.title", "json.a", "@request.body.text", "@request.auth.email", "datetime", "file_many"}
	rights := []string{`'x'`, `'%x_'`, `12`, `null`, `true`, `''`, `number`, `rel_many.title`, `@request.body.number`, `@now`, `select_many`}
	for _, l := range lefts {
		for _, op := range ops {
			for _, r := range rights {
				c = append(c, fmt.Sprintf("%s %s %s", l, op, r))
			}
		}
	}

	return c
}

// handCorpusSize returns the number of hand written entries without the generated matrix.
func handCorpusHandwritten() int {
	return len(handCorpus()) - 12*16*11
}
