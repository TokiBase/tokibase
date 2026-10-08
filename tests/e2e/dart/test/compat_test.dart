import 'dart:async';
import 'dart:io';

import 'package:http/http.dart' as http;
import 'package:pocketbase/pocketbase.dart';
import 'package:test/test.dart';

const adminEmail = 'admin@example.com';
const adminPass = 'adminpass1234';
const userEmail = 'user1@example.com';
const userPass = 'userpass1234';

late final String baseUrl;

PocketBase client() => PocketBase(baseUrl);

Future<ClientException> expectFail(Future<dynamic> f) async {
  try {
    await f;
  } on ClientException catch (e) {
    return e;
  }
  fail('expected ClientException');
}

void main() {
  late PocketBase admin;
  late PocketBase user;

  setUpAll(() {
    final u = Platform.environment['TOKI_URL'];
    if (u == null || u.isEmpty) throw StateError('TOKI_URL is required');
    baseUrl = u;
  });

  test('health check', () async {
    final res = await client().health.check();
    expect(res.code, 200);
  });

  test('superuser auth', () async {
    admin = client();
    final auth =
        await admin.collection('_superusers').authWithPassword(adminEmail, adminPass);
    expect(auth.token, isNotEmpty);
    expect(admin.authStore.isValid, isTrue);
  });

  test('list collections: posts exists with seeded rules', () async {
    final cols = await admin.collections.getFullList();
    final posts = cols.firstWhere((c) => c.name == 'posts',
        orElse: () => fail('posts collection missing'));
    expect(posts.type, 'base');
    expect(posts.listRule, '');
    expect(posts.viewRule, '');
    expect(posts.createRule, '@request.auth.id != ""');
    expect(posts.updateRule, '@request.auth.id != ""');
    expect(posts.deleteRule, '@request.auth.id != ""');
    final title = posts.fields.firstWhere((f) => f.name == 'title');
    expect(title.type, 'text');
    expect(title.required, isTrue);
    expect(posts.fields.any((f) => f.name == 'file' && f.type == 'file'), isTrue);
  });

  test('user auth-with-password', () async {
    user = client();
    final auth =
        await user.collection('users').authWithPassword(userEmail, userPass);
    expect(auth.token, isNotEmpty);
    expect(auth.record.getStringValue('email'), userEmail);
  });

  test('posts CRUD as user', () async {
    final posts = user.collection('posts');
    final rec = await posts.create(body: {'title': 'e2e', 'body': 'b', 'published': true});
    expect(rec.id, isNotEmpty);
    expect(rec.getStringValue('title'), 'e2e');

    final one = await posts.getOne(rec.id);
    expect(one.id, rec.id);

    final list = await posts.getList(page: 1, perPage: 50, filter: 'published = true', sort: '-created');
    expect(list.totalItems, greaterThanOrEqualTo(3));
    expect(list.items.every((r) => r.getBoolValue('published')), isTrue);
    expect(list.items.any((r) => r.id == rec.id), isTrue);
    final dates = list.items.map((r) => r.getStringValue('created')).toList();
    final sorted = [...dates]..sort((a, b) => b.compareTo(a));
    expect(dates, sorted);

    final upd = await posts.update(rec.id, body: {'title': 'e2e-updated'});
    expect(upd.getStringValue('title'), 'e2e-updated');

    await posts.delete(rec.id);
    final e = await expectFail(posts.getOne(rec.id));
    expect(e.statusCode, 404);
  });

  test('unauthenticated create rejected (400), admin API 401, locked collection 403', () async {
    // upstream v0.40.4 answers 400 (not 403) when a non-null createRule does not match
    var e = await expectFail(client().collection('posts').create(body: {'title': 'nope'}));
    expect(e.statusCode, 400);
    e = await expectFail(client().collections.getList(page: 1, perPage: 1));
    expect(e.statusCode, 401);
    e = await expectFail(client().collection('_superusers').getList(page: 1, perPage: 1));
    expect(e.statusCode, 403);
  });

  test('validation error shape (400, data.title.code)', () async {
    final e = await expectFail(user.collection('posts').create(body: {'body': 'no title'}));
    expect(e.statusCode, 400);
    expect(e.response['status'], 400);
    expect(e.response['message'], isA<String>());
    expect(e.response['data']['title']['code'], 'validation_required');
  });

  test('404 error shape', () async {
    final e = await expectFail(client().collection('posts').getOne('doesnotexist00'));
    expect(e.statusCode, 404);
    expect(e.response['status'], 404);
    expect(e.response['data'], isA<Map>());
  });

  test('realtime: subscribe posts/*, create/update/delete events within 5s', () async {
    final posts = user.collection('posts');
    final events = <String, Completer<RecordSubscriptionEvent>>{
      'create': Completer(),
      'update': Completer(),
      'delete': Completer(),
    };
    final unsub = await posts.subscribe('*', (e) {
      if (e.record?.getStringValue('title').startsWith('rt') != true) return;
      final c = events[e.action];
      if (c != null && !c.isCompleted) c.complete(e);
    });
    RecordModel? rec;
    try {
      rec = await posts.create(body: {'title': 'rt'});
      final ev = await events['create']!.future.timeout(const Duration(seconds: 5));
      expect(ev.record!.id, rec.id);

      await posts.update(rec.id, body: {'title': 'rt2'});
      final up = await events['update']!.future.timeout(const Duration(seconds: 5));
      expect(up.record!.getStringValue('title'), 'rt2');

      await posts.delete(rec.id);
      final del = await events['delete']!.future.timeout(const Duration(seconds: 5));
      expect(del.record!.id, rec.id);
      rec = null;
    } finally {
      await unsub();
      if (rec != null) await posts.delete(rec.id).catchError((_) {});
    }
  });

  test('file upload and fetch', () async {
    final rec = await user.collection('posts').create(
      body: {'title': 'with file'},
      files: [http.MultipartFile.fromString('file', 'hello file', filename: 'hello.txt')],
    );
    final name = rec.getStringValue('file');
    expect(name, isNotEmpty);
    final url = user.files.getUrl(rec, name);
    final res = await http.get(url);
    expect(res.statusCode, 200);
    expect(res.body, 'hello file');
    await user.collection('posts').delete(rec.id);
  });

  test('protected file requires file token', () async {
    final col = await admin.collections.create(body: {
      'name': 'dart_protected',
      'type': 'base',
      'listRule': '',
      'viewRule': '',
      'createRule': '',
      'fields': [
        {'name': 'doc', 'type': 'file', 'maxSelect': 1, 'maxSize': 1048576, 'protected': true},
      ],
    });
    try {
      final rec = await admin.collection('dart_protected').create(
        files: [http.MultipartFile.fromString('doc', 'secret', filename: 'secret.txt')],
      );
      final name = rec.getStringValue('doc');
      final anon = await http.get(admin.files.getUrl(rec, name));
      expect(anon.statusCode, 404);
      final token = await admin.files.getToken();
      expect(token, isNotEmpty);
      final ok = await http.get(admin.files.getUrl(rec, name, token: token));
      expect(ok.statusCode, 200);
      expect(ok.body, 'secret');
    } finally {
      await admin.collections.delete(col.id);
    }
  });

  test('auth refresh', () async {
    final before = user.authStore.token;
    final auth = await user.collection('users').authRefresh();
    expect(auth.token, isNotEmpty);
    expect(auth.record.getStringValue('email'), userEmail);
    expect(before, isNotEmpty);
  });

  test('impersonate via superuser', () async {
    final u = await admin.collection('users').getFirstListItem('email="$userEmail"');
    final imp = await admin.collection('users').impersonate(u.id, 120);
    expect(imp.authStore.isValid, isTrue);
    final auth = await imp.collection('users').authRefresh();
    expect(auth.record.id, u.id);
  });

  test('batch endpoint', () async {
    final batch = user.createBatch();
    batch.collection('posts').create(body: {'title': 'batch-1'});
    batch.collection('posts').create(body: {'title': 'batch-2'});
    final res = await batch.send();
    expect(res.length, 2);
    expect(res.every((r) => r.status == 200), isTrue);
    final ids = res.map((r) => (r.body as Map)['id'] as String).toList();
    final list = await user.collection('posts').getList(filter: 'title ~ "batch-"');
    expect(list.items.map((r) => r.id).toSet().containsAll(ids), isTrue);
    for (final id in ids) {
      await user.collection('posts').delete(id);
    }
  });

  test('expand/filter/sort/fields query params', () async {
    // users is a valid expand target through the auth record id relation in a temp collection
    final col = await admin.collections.create(body: {
      'name': 'dart_notes',
      'type': 'base',
      'listRule': '@request.auth.id != "" && owner = @request.auth.id',
      'viewRule': '@request.auth.id != ""',
      'createRule': '@request.auth.id != ""',
      'fields': [
        {'name': 'text', 'type': 'text'},
        {'name': 'owner', 'type': 'relation', 'collectionId': '_pb_users_auth_', 'maxSelect': 1},
      ],
    });
    try {
      final me = user.authStore.record!.id;
      final other = await admin.collection('users').getFirstListItem('email="$userEmail"');
      expect(other.id, me);
      await user.collection('dart_notes').create(body: {'text': 'b', 'owner': me});
      await user.collection('dart_notes').create(body: {'text': 'a', 'owner': me});
      await admin.collection('dart_notes').create(body: {'text': 'c', 'owner': null});

      // filter using @request.auth + sort
      final list = await user.collection('dart_notes').getList(
        filter: 'owner = @request.auth.id',
        sort: 'text',
        expand: 'owner',
        fields: 'id,text,expand.owner.email',
      );
      expect(list.items.map((r) => r.getStringValue('text')).toList(), ['a', 'b']);
      final r0 = list.items.first;
      expect(r0.data.containsKey('owner'), isFalse); // trimmed by fields
      expect(r0.get<String>('expand.owner.email'), userEmail);

      // rule scoping: guest sees nothing / 400+ ; user list rule hides the ownerless record
      final all = await user.collection('dart_notes').getFullList();
      expect(all.length, 2);
    } finally {
      await admin.collections.delete(col.id);
    }
  });

  test('authStore persistence callbacks', () async {
    final saved = <String>[];
    final store = AsyncAuthStore(
      save: (data) async => saved.add(data),
      initial: null,
    );
    final changes = <AuthStoreEvent>[];
    final sub = store.onChange.listen(changes.add);
    final pb = PocketBase(baseUrl, authStore: store);
    await pb.collection('users').authWithPassword(userEmail, userPass);
    await Future<void>.delayed(const Duration(milliseconds: 100));
    expect(saved, isNotEmpty);
    expect(saved.last, contains(store.token));
    expect(changes, isNotEmpty);
    expect(changes.last.token, store.token);
    expect(store.isValid, isTrue);

    // a new store restored from the persisted payload is still valid and usable
    final restored = AsyncAuthStore(save: (_) async {}, initial: saved.last);
    expect(restored.token, store.token);
    final pb2 = PocketBase(baseUrl, authStore: restored);
    final refreshed = await pb2.collection('users').authRefresh();
    expect(refreshed.record.getStringValue('email'), userEmail);

    store.clear();
    await Future<void>.delayed(const Duration(milliseconds: 100));
    expect(store.isValid, isFalse);
    expect(saved.last, '');
    await sub.cancel();
  });
}
