import { test } from "node:test";
import assert from "node:assert/strict";
import { EventSource } from "eventsource";
import PocketBase from "pocketbase";

globalThis.EventSource ??= EventSource; // PocketBase SDK realtime needs it in Node

const URL_ = process.env.TOKI_URL;
assert.ok(URL_, "TOKI_URL is required");

const ADMIN = { email: "admin@example.com", pass: "adminpass1234" };
const USER = { email: "user1@example.com", pass: "userpass1234" };

const client = () => {
  const pb = new PocketBase(URL_);
  pb.autoCancellation(false);
  return pb;
};

const state = {};

test("health check", async () => {
  const res = await client().health.check();
  assert.equal(res.code, 200);
});

test("superuser auth", async () => {
  const pb = client();
  const auth = await pb.collection("_superusers").authWithPassword(ADMIN.email, ADMIN.pass);
  assert.ok(auth.token);
  assert.ok(pb.authStore.isValid);
  state.admin = pb;
});

test("list collections: posts exists with seeded rules", async () => {
  const cols = await state.admin.collections.getFullList();
  const posts = cols.find((c) => c.name === "posts");
  assert.ok(posts, "posts collection missing");
  assert.equal(posts.type, "base");
  assert.equal(posts.listRule, "");
  assert.equal(posts.viewRule, "");
  assert.equal(posts.createRule, '@request.auth.id != ""');
  assert.equal(posts.updateRule, '@request.auth.id != ""');
  assert.equal(posts.deleteRule, '@request.auth.id != ""');
  const title = posts.fields.find((f) => f.name === "title");
  assert.equal(title.type, "text");
  assert.equal(title.required, true);
  assert.ok(posts.fields.find((f) => f.name === "file" && f.type === "file"));
});

test("user auth-with-password", async () => {
  const pb = client();
  const auth = await pb.collection("users").authWithPassword(USER.email, USER.pass);
  assert.ok(auth.token);
  assert.equal(auth.record.email, USER.email);
  state.user = pb;
});

test("posts CRUD as user", async () => {
  const pb = state.user;
  const rec = await pb.collection("posts").create({ title: "e2e", body: "b", published: true });
  assert.ok(rec.id);
  assert.equal(rec.title, "e2e");

  const one = await pb.collection("posts").getOne(rec.id);
  assert.equal(one.id, rec.id);

  const list = await pb.collection("posts").getList(1, 50, { filter: "published = true", sort: "-created" });
  assert.ok(list.totalItems >= 3);
  assert.ok(list.items.every((r) => r.published === true));
  assert.ok(list.items.some((r) => r.id === rec.id));
  const dates = list.items.map((r) => r.created);
  assert.deepEqual(dates, [...dates].sort().reverse());

  const upd = await pb.collection("posts").update(rec.id, { title: "e2e-updated" });
  assert.equal(upd.title, "e2e-updated");

  await pb.collection("posts").delete(rec.id);
  await assert.rejects(pb.collection("posts").getOne(rec.id), (e) => e.status === 404);
});

test("unauthenticated create is rejected (400, create rule fails) admin API 401, locked collection 403", async () => {
  // upstream v0.40.4 answers 400 (not 403) when a non-null createRule does not match
  await assert.rejects(client().collection("posts").create({ title: "nope" }), (e) => e.status === 400);
  // anonymous: 401 on superuser-only endpoints, 403 on locked (null-rule) collections
  await assert.rejects(client().collections.getList(1, 1), (e) => e.status === 401);
  await assert.rejects(client().collection("_superusers").getList(1, 1), (e) => e.status === 403);
});

test("validation error shape (400, data.title.code)", async () => {
  await assert.rejects(state.user.collection("posts").create({ body: "no title" }), (e) => {
    assert.equal(e.status, 400);
    assert.equal(e.response.data.title.code, "validation_required");
    return true;
  });
});

test("realtime: subscribe posts/*, event within 5s", async () => {
  const pb = state.user;
  let resolve;
  const got = new Promise((r) => (resolve = r));
  const unsub = await pb.collection("posts").subscribe("*", (e) => {
    if (e.action === "create" && e.record.title === "rt") resolve(e);
  });
  let rec;
  try {
    rec = await pb.collection("posts").create({ title: "rt" });
    const ev = await Promise.race([
      got,
      new Promise((_, rej) => setTimeout(() => rej(new Error("no realtime event in 5s")), 5000)),
    ]);
    assert.equal(ev.record.id, rec.id);
  } finally {
    await unsub();
    if (rec) await pb.collection("posts").delete(rec.id).catch(() => {});
  }
});

test("file upload and fetch", async () => {
  const pb = state.user;
  const form = new FormData();
  form.append("title", "with file");
  form.append("file", new Blob(["hello file"], { type: "text/plain" }), "hello.txt");
  const rec = await pb.collection("posts").create(form);
  assert.ok(rec.file);
  const url = pb.files.getURL(rec, rec.file);
  const res = await fetch(url);
  assert.equal(res.status, 200);
  assert.equal(await res.text(), "hello file");
  await pb.collection("posts").delete(rec.id);
});

test("auth refresh", async () => {
  const pb = state.user;
  const before = pb.authStore.token;
  const auth = await pb.collection("users").authRefresh();
  assert.ok(auth.token);
  assert.equal(auth.record.email, USER.email);
  assert.ok(before);
});

test("impersonate via superuser", async () => {
  const admin = state.admin;
  const u = await admin.collection("users").getFirstListItem(`email="${USER.email}"`);
  const imp = await admin.collection("users").impersonate(u.id, 120);
  assert.ok(imp.authStore.isValid);
  const auth = await imp.collection("users").authRefresh();
  assert.equal(auth.record.id, u.id);
});
