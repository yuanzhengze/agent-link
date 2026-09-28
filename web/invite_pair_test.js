"use strict";

const assert = require("assert");
const path = require("path");
const { parseInvitePair, invitePairText } = require(path.join(__dirname, "teams.js"));

function eq(actual, teamID, inviteCode) {
  assert.deepStrictEqual(actual, { teamID: teamID, inviteCode: inviteCode });
}

eq(parseInvitePair(invitePairText("tm_abc", "inv_def")), "tm_abc", "inv_def");
eq(parseInvitePair("Team ID: tm_abc\nInvite code: inv_def"), "tm_abc", "inv_def");
eq(parseInvitePair("Team ID: tm_abc\r\nInvite code: inv_def"), "tm_abc", "inv_def");
eq(parseInvitePair("team id: tm_abc\nINVITE CODE: inv_def"), "tm_abc", "inv_def");
eq(parseInvitePair("TeamID:tm_abc Invite code:inv_def"), "tm_abc", "inv_def");
eq(parseInvitePair("邀请码: inv_def\n团队 ID: tm_abc"), "tm_abc", "inv_def");
eq(parseInvitePair("团队 ID：tm_abc\n邀请码：inv_def"), "tm_abc", "inv_def");
eq(parseInvitePair("团队ID:tm_abc\n邀请码:inv_def"), "tm_abc", "inv_def");
eq(parseInvitePair("Team ID: tm_abc\n邀请码：inv_def"), "tm_abc", "inv_def");
eq(parseInvitePair("请加入\nTeam ID: tm_abc,\nInvite code: inv_def。\n谢谢"), "tm_abc", "inv_def");
eq(parseInvitePair("Team ID:\u00a0tm_abc\nInvite code:\u3000inv_def"), "tm_abc", "inv_def");

assert.strictEqual(parseInvitePair("inv_def"), null);
assert.strictEqual(parseInvitePair("Team ID: tm_abc"), null);
assert.strictEqual(parseInvitePair("邀请码：inv_def"), null);
assert.strictEqual(parseInvitePair("加入需要团队 ID 和邀请码"), null);
assert.strictEqual(parseInvitePair(""), null);
assert.strictEqual(parseInvitePair(null), null);

console.log("invite pair parser ok");
