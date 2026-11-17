#!/usr/bin/env node
'use strict';
// xinxin-link TOTP —— 60s 一步 / 位数 5/6/7 随步随机 / 有效 3 分钟
// 两台机器用同一 secret + 同一算法，同一分钟必然算出同一个 (位数, 数字)
// 零依赖：只用 node 内置 crypto

const crypto = require('crypto');

const PERIOD = 60;   // 每 60 秒换一次
const BAND = 3;      // 有效期 3 分钟：当前步 + 前 2 步
const D_MIN = 5, D_MAX = 7;
const B32 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';

function b32decode(s) {
  s = String(s).toUpperCase().replace(/[^A-Z2-7]/g, '');
  let bits = '';
  for (const c of s) bits += B32.indexOf(c).toString(2).padStart(5, '0');
  const out = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8), 2));
  return Buffer.from(out);
}

function stepAt(sec) {
  return Math.floor((sec === undefined ? Date.now() / 1000 : sec) / PERIOD);
}

// tag: 0=取数字串  1=取位数
function mac(secret, step, tag) {
  const b = Buffer.alloc(9);
  b.writeBigUInt64BE(BigInt(step), 0);
  b[8] = tag;
  return crypto.createHmac('sha1', secret).update(b).digest();
}

function digitsFor(secret, step) {
  const d = mac(secret, step, 1);
  return D_MIN + (d[0] % (D_MAX - D_MIN + 1));   // 5 / 6 / 7
}

function codeFor(secret, step) {
  const d = mac(secret, step, 0);
  const digits = digitsFor(secret, step);
  const off = d[19] & 0x0f;
  const bin = ((d[off] & 0x7f) << 24) | (d[off + 1] << 16) | (d[off + 2] << 8) | d[off + 3];
  return String(bin % Math.pow(10, digits)).padStart(digits, '0');
}

function current(secretB32, nowSec) {
  const s = b32decode(secretB32);
  const step = stepAt(nowSec);
  return { step, digits: digitsFor(s, step), code: codeFor(s, step) };
}

// 返回 {ok, step, skew, reason}
function verify(secretB32, token, nowSec) {
  const tok = String(token == null ? '' : token).trim();
  if (!/^\d{5,7}$/.test(tok)) return { ok: false, reason: 'format' };
  const s = b32decode(secretB32);
  const step = stepAt(nowSec);
  for (let k = 0; k < BAND; k++) {
    const st = step - k;
    const c = codeFor(s, st);
    if (c.length === tok.length && c === tok) return { ok: true, step: st, skew: k };
  }
  return { ok: false, reason: 'mismatch' };
}

function newSecret(bytes) {
  const buf = crypto.randomBytes(bytes || 32);
  let bits = '';
  for (const b of buf) bits += b.toString(2).padStart(8, '0');
  let out = '';
  for (let i = 0; i + 5 <= bits.length; i += 5) out += B32[parseInt(bits.slice(i, i + 5), 2)];
  return out;
}

module.exports = { current, verify, codeFor, digitsFor, stepAt, b32decode, newSecret, PERIOD, BAND };

if (require.main === module) {
  const a = process.argv.slice(2);
  if (a[0] === 'gen') { console.log(newSecret(32)); process.exit(0); }
  const sec = a[1] || process.env.LINK_SECRET;
  if (a[0] === 'now') { console.log(JSON.stringify(current(sec))); process.exit(0); }
  if (a[0] === 'check') { console.log(JSON.stringify(verify(sec, a[2]))); process.exit(0); }
  console.log('usage: totp.js gen | now <secret> | check <secret> <token>');
}
