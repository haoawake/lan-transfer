/* 文件传输助手 · 网页端
 *
 * 整个文件只用 ES5：不用箭头函数、let/const、模板字符串、Promise、fetch，
 * 网络请求全部走 XMLHttpRequest（它还能报告上传进度）。
 * 这样 iOS 9 以后的 Safari、安卓 5 以后的各种浏览器和 App 内置浏览器都能用。
 */
(function () {
  'use strict';

  var CHUNK = 16 * 1024 * 1024; // 每次请求传 16 MB；断线只用重传这一块
  var PARALLEL = 3; // 同时传几个文件
  var STALL_MS = 15000; // 这么久没有一点进度，就当连接卡死了：问清楚进度后接着传
  var MAX_TRIES = 60; // 连续失败这么多次（约 5 分钟）才放弃；每成功一块就重新计数

  var UA = navigator.userAgent || '';
  var isIOS = /iPad|iPhone|iPod/.test(UA) || (/Macintosh/.test(UA) && navigator.maxTouchPoints > 1);
  var isApple = isIOS || (/Macintosh/.test(UA) && /Safari/.test(UA) && !/Chrome|Chromium|Edg|Firefox/.test(UA));
  var isAndroid = /Android/i.test(UA);
  var inApp = /MicroMessenger|\bQQ\/|DingTalk|Weibo|AlipayClient|Lark|FeiShu|wxwork/i.test(UA);
  var inWeChat = /MicroMessenger/i.test(UA);

  // ---------------------------------------------------------------- 小工具

  function $(id) { return document.getElementById(id); }

  var ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return ESC[c]; }); }

  function parseJSON(s) { try { return JSON.parse(s); } catch (e) { return null; } }

  function rid() {
    var chars = 'abcdefghijklmnopqrstuvwxyz0123456789', out = '', rnd = null, i;
    try { rnd = new Uint8Array(20); (window.crypto || window.msCrypto).getRandomValues(rnd); } catch (e) { rnd = null; }
    for (i = 0; i < 20; i++) out += chars.charAt((rnd ? rnd[i] : Math.floor(Math.random() * 256)) % 36);
    return out;
  }

  function ls(k, v) {
    try {
      if (v === undefined) return window.localStorage.getItem(k);
      window.localStorage.setItem(k, v);
    } catch (e) {}
    return null;
  }

  function pad(n) { return n < 10 ? '0' + n : '' + n; }

  function fmtSize(n) {
    if (!(n > 0)) return '0 B';
    if (n < 1024) return Math.round(n) + ' B';
    var u = ['KB', 'MB', 'GB', 'TB'], i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < 3);
    return (n >= 100 ? n.toFixed(0) : n >= 10 ? n.toFixed(1) : n.toFixed(2)) + ' ' + u[i];
  }

  function fmtDur(s) {
    s = Math.round(s);
    var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), x = s % 60;
    return (h ? h + ':' + pad(m) : m) + ':' + pad(x);
  }

  function fmtEta(s) {
    if (!isFinite(s) || s <= 0) return '';
    if (s < 60) return '剩 ' + Math.ceil(s) + ' 秒';
    if (s < 3600) return '剩 ' + Math.ceil(s / 60) + ' 分钟';
    return '剩 ' + (s / 3600).toFixed(1) + ' 小时';
  }

  function fmtTime(t) { var d = new Date(t); return pad(d.getHours()) + ':' + pad(d.getMinutes()); }

  function dayStart(t) { var d = new Date(t); return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime(); }

  function dayLabel(t) {
    var d = new Date(t), now = new Date();
    var diff = Math.round((dayStart(now.getTime()) - dayStart(t)) / 86400000);
    if (diff === 0) return '今天';
    if (diff === 1) return '昨天';
    if (d.getFullYear() === now.getFullYear()) return (d.getMonth() + 1) + '月' + d.getDate() + '日';
    return d.getFullYear() + '年' + (d.getMonth() + 1) + '月' + d.getDate() + '日';
  }

  function setText(el, s) { if ('textContent' in el) el.textContent = s; else el.innerText = s; }

  var toastTimer = null;
  function toast(msg, ms) {
    var t = $('toast');
    setText(t, msg);
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.hidden = true; }, ms || 2400);
  }

  // ---------------------------------------------------------------- 这台设备

  function guessName() {
    if (/iPad/.test(UA) || (/Macintosh/.test(UA) && navigator.maxTouchPoints > 1)) return 'iPad';
    if (/iPhone/.test(UA)) return 'iPhone';
    if (/iPod/.test(UA)) return 'iPod';
    if (isAndroid) {
      // 老的 UA 里带着型号，比如 "Android 9; MI 8 Build/…"；新版 Chrome 统一写成 "K"
      var m = UA.match(/Android[^;)]*;\s*(?:[a-z]{2}[-_][a-z]{2};\s*)?([^;)]+?)(?:\s+Build\/|\s*\)|;)/i);
      var model = m ? m[1].replace(/^\s+|\s+$/g, '') : '';
      if (model && model.length > 1 && model.length <= 24 && !/^(wv|U|K|Linux)$/i.test(model)) return model;
      return '安卓手机';
    }
    if (/Windows/.test(UA)) return 'Windows 电脑';
    if (/Macintosh|Mac OS X/.test(UA)) return 'Mac';
    if (/CrOS/.test(UA)) return 'Chromebook';
    if (/Linux/.test(UA)) return 'Linux 电脑';
    return '新设备';
  }

  var me = { id: ls('lt.id'), name: ls('lt.name') || guessName() };
  if (!me.id || !/^[A-Za-z0-9_-]{8,64}$/.test(me.id)) {
    me.id = rid();
    ls('lt.id', me.id);
  }

  // ---------------------------------------------------------------- 网络请求

  function api(method, url, body, cb) {
    var x = new XMLHttpRequest();
    x.open(method, url, true);
    x.setRequestHeader('X-LT', '1');
    x.setRequestHeader('X-Device-Id', me.id);
    x.setRequestHeader('X-Device-Name', encodeURIComponent(me.name));
    var data = null;
    if (body != null) {
      if (window.Blob && body instanceof window.Blob) data = body;
      else {
        x.setRequestHeader('Content-Type', 'application/json');
        data = JSON.stringify(body);
      }
    }
    x.onreadystatechange = function () {
      if (x.readyState !== 4) return;
      var d = parseJSON(x.responseText) || {};
      if (x.status === 401) needLogin();
      if (cb) cb(x.status, d);
    };
    x.send(data);
    return x;
  }

  // ---------------------------------------------------------------- 文件类型

  var GROUPS = [
    ['image', '#13a8c4', 'jpg jpeg png gif webp bmp heic heif avif svg ico tif tiff raw dng psd'],
    ['video', '#f08c00', 'mp4 mov m4v webm mkv avi 3gp flv wmv ts mts m2ts rmvb rm vob'],
    ['audio', '#e64980', 'mp3 m4a aac wav flac ogg opus amr wma ape'],
    ['pdf', '#e5484d', 'pdf'],
    ['doc', '#2f6bff', 'doc docx pages rtf odt wps'],
    ['xls', '#0e9f57', 'xls xlsx csv numbers ods et'],
    ['ppt', '#f0743a', 'ppt pptx key odp dps'],
    ['zip', '#8b5cf6', 'zip rar 7z tar gz tgz bz2 xz zst iso dmg'],
    ['app', '#14a37f', 'apk exe msi ipa deb rpm appimage pkg'],
    ['text', '#64748b', 'txt md log json xml yaml yml ini conf html htm css js ts py java c cpp h go rs sh bat ps1 sql']
  ];
  var EXT_GROUP = {};
  (function () {
    for (var i = 0; i < GROUPS.length; i++) {
      var exts = GROUPS[i][2].split(' ');
      for (var j = 0; j < exts.length; j++) EXT_GROUP[exts[j]] = GROUPS[i];
    }
  })();

  function setOf(s) { var o = {}, a = s.split(' '); for (var i = 0; i < a.length; i++) o[a[i]] = 1; return o; }
  var IMG_OK = setOf('jpg jpeg png gif webp bmp avif ico svg');
  var VID_OK = setOf('mp4 m4v mov webm');
  var AUD_OK = setOf('mp3 m4a aac wav ogg opus flac');

  function extOf(name) {
    name = name || '';
    var i = name.lastIndexOf('.');
    return i > 0 && i < name.length - 1 ? name.slice(i + 1).toLowerCase() : '';
  }

  // 网页里怎么展示：text / image / video / audio / pdf / file
  function kindOf(it) {
    if (it.type === 'text') return 'text';
    var e = extOf(it.name);
    if (IMG_OK[e] || (isApple && (e === 'heic' || e === 'heif'))) return 'image';
    if (VID_OK[e]) return 'video';
    if (AUD_OK[e]) return 'audio';
    if (e === 'pdf') return 'pdf';
    return 'file';
  }

  function groupOf(it) {
    var g = EXT_GROUP[extOf(it.name)];
    if (g) return g;
    var m = it.mime || '';
    if (/^image\//.test(m)) return GROUPS[0];
    if (/^video\//.test(m)) return GROUPS[1];
    if (/^audio\//.test(m)) return GROUPS[2];
    return ['file', '#94a3b8'];
  }

  var RISKY = setOf('exe msi bat cmd com scr ps1 vbs vbe js jse wsf wsh lnk url reg hta cpl msc jar pif sh command app pkg');

  function fileUrl(it, dl) {
    return '/f/' + it.id + '/' + encodeURIComponent(it.name) + '?s=' + (it.sig || '') + (dl ? '&dl=1' : '');
  }
  function thumbUrl(it) { return '/t/' + it.id + '?s=' + (it.sig || ''); }

  // ---------------------------------------------------------------- 状态

  var items = {}; // id → 记录（服务端的，或者本机正在发送的）
  var uploads = {}; // id → 正在上传的文件
  var seq = 0; // 本机发送的先后顺序
  var isHost = false, host = null, online = [], hasMore = false, loadedOlder = false;
  var ready = false, connected = false, version = '';
  var unread = 0;

  var list = $('list'), scroller = $('scroller');

  function key(it) { return it.pending ? 9e15 + it.seq : it.time; }

  function nearBottom() { return scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 160; }
  function toBottom() { scroller.scrollTop = list.firstChild ? scroller.scrollHeight : 0; }

  // ---------------------------------------------------------------- 渲染

  function boxSize(it) {
    var maxW = Math.min(list.clientWidth > 600 ? 320 : 260, Math.round((list.clientWidth || 360) * 0.66)), maxH = 340;
    var w = it.w, h = it.h;
    if (!w || !h) return { w: Math.min(maxW, 220), h: 165 };
    var bw = maxW, bh = Math.round((maxW * h) / w);
    if (bh > maxH) { bh = maxH; bw = Math.round((maxH * w) / h); }
    return { w: Math.max(bw, 110), h: Math.max(bh, 90) };
  }

  function linkify(t) {
    var re = /https?:\/\/[^\s<>"'，。！？；：、（）【】《》「」]+/g, out = '', last = 0, m;
    while ((m = re.exec(t))) {
      var u = m[0].replace(/[),.;:!?\]]+$/, '');
      out += esc(t.slice(last, m.index)) + '<a href="' + esc(u) + '" target="_blank" rel="noopener noreferrer">' + esc(u) + '</a>';
      last = m.index + u.length;
      re.lastIndex = last;
    }
    return out + esc(t.slice(last));
  }

  function act(name, label, cls) {
    return '<button type="button" class="act' + (cls ? ' ' + cls : '') + '" data-act="' + name + '">' + label + '</button>';
  }

  function mediaHTML(it, k) {
    var sz = boxSize(it), src = '', e = extOf(it.name);
    if (it._preview) src = it._preview;
    else if (!it.pending && e === 'gif' && it.size <= 8 * 1048576) src = fileUrl(it); // 小 GIF 直接显示，保留动画
    else if (it.thumb) src = thumbUrl(it);
    else if (!it.pending && k === 'image' && it.size <= 4 * 1048576) src = fileUrl(it);
    var h = '<div class="media ' + k + '" role="button" tabindex="0" data-act="view" style="width:' + sz.w + 'px;height:' + sz.h + 'px">';
    if (src) h += '<img src="' + esc(src) + '" alt="" loading="lazy" draggable="false">';
    else h += '<div class="ph">' + (k === 'video' ? '视频' : '图片') + ' · ' + fmtSize(it.size) + '</div>';
    if (k === 'video') {
      h += '<span class="play"></span>';
      if (it.dur) h += '<span class="badge">' + fmtDur(it.dur) + '</span>';
    }
    if (it.pending) h += '<div class="ov"><div class="progress"><i></i></div><div class="status"></div></div>';
    return h + '</div>';
  }

  function fileHTML(it, k) {
    var g = groupOf(it), e = extOf(it.name).toUpperCase().slice(0, 4) || '文件';
    var hint = '';
    if (!it.pending) {
      if (isHost) hint = RISKY[extOf(it.name)] ? '点击在文件夹中显示' : '点击用电脑打开';
      else hint = k === 'audio' ? '点击播放' : k === 'pdf' ? '点击查看' : '点击下载';
    }
    var h = '<div class="file" role="button" tabindex="0"' + (it.pending ? '' : ' data-act="file"') + '>';
    h += '<div class="ficon" style="background:' + g[1] + '">' + esc(e) + '</div>';
    h += '<div class="finfo"><div class="fname">' + esc(it.name) + '</div>';
    if (it.pending) h += '<div class="progress"><i></i></div><div class="status"></div>';
    else h += '<div class="fsub">' + fmtSize(it.size) + ' · ' + hint + '</div>';
    return h + '</div></div>';
  }

  function metaHTML(it, mine) {
    var a = [];
    if (it.pending) {
      var u = uploads[it.id];
      if (it.type === 'text') { if (it.failed) a.push(act('resend', '重发')); }
      else {
        if (u && u.state === 'error') a.push(act('retry', '重试'));
        a.push(act('cancel', '取消'));
      }
    } else if (it.type === 'text') {
      a.push(act('copy', '复制'));
      a.push(act('del', '删除', 'danger'));
    } else {
      if (isHost) {
        a.push(act('open', '打开'));
        a.push(act('reveal', '<span class="hide-sm">在文件夹中</span>显示'));
      } else a.push(act('dl', '下载'));
      a.push(act('del', '删除', 'danger'));
    }
    var who = mine ? '' : '<span class="who">' + esc(it.from || '') + '</span>';
    var t = it.pending && it.type === 'text' && !it.failed ? '发送中…' : it.failed ? '没发出去' : fmtTime(it.time);
    return '<div class="meta">' + who + '<span class="t">' + t + '</span><span class="acts">' + a.join('') + '</span></div>';
  }

  function itemHTML(it) {
    var k = kindOf(it), mine = it.fromId === me.id;
    var h = '<div class="day"></div>';
    if (it.type === 'text') h += '<div class="bubble">' + linkify(it.text || '') + '</div>';
    else if (k === 'image' || k === 'video') h += mediaHTML(it, k);
    else h += fileHTML(it, k);
    return h + metaHTML(it, mine);
  }

  // 记录有没有变化：没变就不重画，免得图片闪一下
  function sigOf(it) {
    var u = uploads[it.id];
    return [it.id, it.pending ? 1 : 0, it.failed ? 1 : 0, it.thumb ? 1 : 0, it.w, it.h, it.dur, it.name, it.from,
      it._preview || '', u ? u.state : '', isHost ? 1 : 0, it.sig || ''].join('|');
  }

  // 同一天的第一条消息上面显示日期
  function fixDay(el) {
    if (!el || !el.getAttribute) return;
    var it = items[el.getAttribute('data-id')], d = el.firstChild;
    if (!it || !d) return;
    var prev = el.previousSibling, pit = prev && prev.getAttribute ? items[prev.getAttribute('data-id')] : null;
    var t = it.pending ? Date.now() : it.time;
    var show = !pit || dayStart(pit.pending ? Date.now() : pit.time) !== dayStart(t);
    d.style.display = show ? '' : 'none';
    if (show) setText(d, dayLabel(t));
  }

  function placeEl(it, el) {
    if (el.parentNode) el.parentNode.removeChild(el);
    var k = key(it), node = list.lastChild;
    while (node) {
      var o = items[node.getAttribute('data-id')];
      if (!o || key(o) <= k) break;
      node = node.previousSibling;
    }
    list.insertBefore(el, node ? node.nextSibling : list.firstChild);
  }

  function upsert(it, batch) {
    var old = items[it.id];
    if (old && old !== it) {
      // 自己刚发完的图片：继续用本机生成的预览，省得再从电脑下载一遍缩略图
      if (old._preview && !it._preview) it._preview = old._preview;
      if (!it.w && old.w) { it.w = old.w; it.h = old.h; }
      if (!it.dur && old.dur) it.dur = old.dur;
    }
    var mine = it.fromId === me.id;
    if (!old && !mine && !it.pending && document.hidden && ready) {
      unread++;
      document.title = '(' + unread + ') 文件传输助手';
    }
    items[it.id] = it;
    var el = $('m-' + it.id), fresh = !el;
    var stick = fresh && !batch && (mine || nearBottom());
    if (fresh) {
      el = document.createElement('div');
      el.id = 'm-' + it.id;
      el.setAttribute('data-id', it.id);
    }
    var sig = sigOf(it);
    if (el._sig !== sig) {
      el._sig = sig;
      el.className = 'msg ' + (mine ? 'me' : 'other') + (it.pending ? ' pending' : '');
      el.innerHTML = itemHTML(it);
    }
    if (fresh || key(old) !== key(it)) placeEl(it, el);
    fixDay(el);
    fixDay(el.nextSibling);
    if (uploads[it.id]) paintUpload(uploads[it.id]);
    if (!batch) {
      updateEmpty();
      if (stick) toBottom();
    }
  }

  function removeItem(id) {
    var el = $('m-' + id);
    delete items[id];
    if (el && el.parentNode) {
      var next = el.nextSibling;
      el.parentNode.removeChild(el);
      fixDay(next);
    }
    updateEmpty();
  }

  function rerenderAll() {
    var nodes = list.childNodes;
    for (var i = 0; i < nodes.length; i++) {
      var it = items[nodes[i].getAttribute('data-id')];
      if (it) { nodes[i]._sig = ''; upsert(it, true); }
    }
    updateEmpty();
  }

  function applySync(d) {
    if (d.version) version = d.version;
    setHost(d.me && d.me.host, d.host);
    var arr = d.items || [], keep = {}, i, id;
    for (i = 0; i < arr.length; i++) keep[arr[i].id] = 1;
    // 同步里只有最新的一段：这段时间范围内本机有、同步里没有的，就是在别处被删了
    var minT = d.hasMore && arr.length ? arr[0].time : -1;
    var drop = [];
    for (id in items) {
      if (items.hasOwnProperty(id) && !items[id].pending && !keep[id] && items[id].time >= minT) drop.push(id);
    }
    for (i = 0; i < drop.length; i++) removeItem(drop[i]);
    var first = !ready || !list.firstChild, near = nearBottom();
    for (i = 0; i < arr.length; i++) upsert(arr[i], true);
    if (!loadedOlder) hasMore = !!d.hasMore;
    $('more').hidden = !hasMore;
    ready = true;
    setOnline(d.online || online);
    updateEmpty();
    if (first || near) toBottom();
  }

  // ---------------------------------------------------------------- 空状态、扫码卡片

  var qrIdx = 0, qrVer = 0, qrAll = false;

  function qrCardHTML(inline) {
    var urls = (host && host.urls) || [];
    var cls = 'qr-card' + (inline ? ' glass wide' : '');
    if (!urls.length) {
      return '<div class="' + cls + '"><h2>没找到局域网地址</h2><p class="lead">这台电脑好像没有连上 Wi-Fi 或网线。连上以后，这里会自动出现二维码。</p></div>';
    }
    if (qrIdx >= urls.length) qrIdx = 0;
    var u = urls[qrIdx], i, chips = '', shown = 0, hidden = 0;
    for (i = 0; i < urls.length; i++) {
      // 虚拟机、VPN 的网卡手机一般连不到，默认收起来
      if (urls[i].virtual && !qrAll && i !== qrIdx) { hidden++; continue; }
      shown++;
      chips += '<button type="button" class="chip' + (i === qrIdx ? ' on' : '') + '" data-qr="' + i + '">' + esc(urls[i].ip) + ' · ' + esc(urls[i].iface) + '</button>';
    }
    var h = '<div class="' + cls + '">';
    h += '<h2>用手机扫一扫</h2><p class="lead">手机和这台电脑连同一个 Wi-Fi，用相机或浏览器扫码</p>';
    h += '<div class="qr-body"><div class="qr-left">';
    h += '<img class="qr-img" alt="二维码" src="/api/qr?ip=' + encodeURIComponent(u.ip) + '&v=' + qrVer + '">';
    h += '<div class="addr">' + esc(u.url.replace(/^https?:\/\//, '')) + '</div>';
    if (!host.noAuth) h += '<div class="code-line">手动输入地址时的访问码 <b>' + esc(host.code) + '</b></div>';
    h += '</div><div class="qr-right">';
    if (shown > 1) h += '<div class="chips">' + chips + '</div>';
    if (hidden) h += '<button type="button" class="linkish" data-sa="qr-all">还有 ' + hidden + ' 个地址（虚拟网卡、VPN 等）</button>';
    h += '<div class="copy-row"><button type="button" class="btn" data-sa="copy-link">复制链接</button></div>';
    h += '<ul class="tips">';
    h += '<li>扫不上？换一个地址试试，或者在手机浏览器里直接输入上面的地址</li>';
    h += '<li>用微信扫码的话，进去后点右上角「···」→「在浏览器打开」才能下载文件</li>';
    h += '<li>电脑弹出防火墙提示时要点「允许」；校园网、公共 Wi-Fi 常常禁止设备互连，可以让电脑连上手机热点再试</li>';
    h += '</ul></div></div></div>';
    return h;
  }

  function updateEmpty() {
    var el = $('empty');
    if (list.firstChild || !ready) { el.hidden = true; return; }
    var html = isHost
      ? qrCardHTML(true)
      : '<div class="welcome"><img src="/icon.svg" alt=""><b>已连上电脑</b>发一张图片或一个文件试试，<br>它会马上出现在电脑和其他设备上。</div>';
    if (el._html !== html) { el.innerHTML = html; el._html = html; }
    el.hidden = false;
  }

  function refreshHostUI() {
    updateEmpty();
    if (sheetKind === 'qr') showQR();
    else if (sheetKind === 'settings') showSettings();
    updateBanner();
  }

  function setHost(flag, info) {
    var changed = isHost !== !!flag;
    isHost = !!flag;
    if (info) host = info;
    $('btn-qr').hidden = !isHost;
    if (changed) rerenderAll();
    updateBanner();
  }

  // ---------------------------------------------------------------- 在线设备、连接状态

  function setOnline(arr) {
    online = arr || [];
    var others = 0;
    for (var i = 0; i < online.length; i++) if (online[i].id !== me.id) others++;
    var dot = $('online-dot'), txt = $('online-text');
    if (!connected) { dot.className = 'dot'; setText(txt, '连接中…'); return; }
    if (!others) { dot.className = 'dot wait'; setText(txt, isHost ? '等待手机连接' : '只有本机在线'); }
    else { dot.className = 'dot on'; setText(txt, others + 1 + ' 台设备在线'); }
  }

  var lostTimer = null, bannerDismissed = false;
  function setConnected(ok) {
    if (connected === ok) return;
    connected = ok;
    setOnline(online);
    clearTimeout(lostTimer);
    if (ok) updateBanner();
    else lostTimer = setTimeout(updateBanner, 2500);
  }

  function updateBanner() {
    var b = $('banner');
    if (!connected && ready) {
      b.className = 'banner err';
      b.innerHTML = '和电脑的连接断开了，正在重新连接…（电脑上的程序关掉了吗？手机还连着同一个 Wi-Fi 吗？）';
      b.hidden = false;
    } else if (inApp && !bannerDismissed) {
      b.className = 'banner';
      b.innerHTML = (inWeChat ? '现在是在微信里打开的' : '现在是在 App 内置的浏览器里打开的') +
        '：可以发文件，但下载很可能被拦截。点右上角「···」→「在浏览器打开」就都能用了。<button type="button" class="x" id="banner-x">×</button>';
      b.hidden = false;
      $('banner-x').onclick = function () { bannerDismissed = true; updateBanner(); };
    } else if (isHost && host && host.free > 0 && host.free < 1073741824) {
      // 只在电脑上提示：接收文件夹所在的盘快满了
      b.className = 'banner';
      b.innerHTML = '接收文件夹所在的硬盘只剩 ' + fmtSize(host.free) + ' 了，大文件会传不进来。<button type="button" class="linkish" data-sa="settings">换个文件夹</button>';
      b.hidden = false;
    } else b.hidden = true;
  }

  // ---------------------------------------------------------------- 实时同步（SSE，没有就轮询）

  var es = null, lastEvent = 0;

  function connect() {
    if (es) { try { es.close(); } catch (e) {} es = null; }
    if (loginShown) return;
    if (!window.EventSource) { poll(); return; }
    var src = new EventSource('/api/events?did=' + encodeURIComponent(me.id) + '&dname=' + encodeURIComponent(me.name));
    es = src;
    lastEvent = Date.now();
    function on(name, fn) {
      src.addEventListener(name, function (e) {
        if (src !== es) return;
        lastEvent = Date.now();
        var d = parseJSON(e.data);
        if (d) fn(d);
      });
    }
    on('sync', function (d) { setConnected(true); applySync(d); });
    on('item', function (d) { upsert(d); });
    on('del', function (d) {
      for (var i = 0; i < (d.ids || []).length; i++) {
        removeItem(d.ids[i]);
        if (vOpen && vList[vIdx] && vList[vIdx].id === d.ids[i]) closeViewer();
      }
    });
    on('online', function (d) { setOnline(d); });
    on('host', function (d) { host = d; qrVer++; refreshHostUI(); });
    on('ping', function () { setConnected(true); });
    src.onerror = function () {
      if (src !== es) return;
      setConnected(false);
      // CLOSED 说明浏览器不会自己重连了（比如访问码换了，服务器回了 401）
      if (src.readyState === 2) {
        es = null;
        setTimeout(recheck, 1500);
      }
    };
  }

  function recheck() {
    api('GET', '/api/sync', null, function (st, d) {
      if (st === 200) { setConnected(true); applySync(d); connect(); }
      else if (st !== 401) setTimeout(recheck, 3000);
    });
  }

  function poll() {
    api('GET', '/api/sync', null, function (st, d) {
      if (st === 200) { setConnected(true); applySync(d); }
      else setConnected(false);
      if (st !== 401) setTimeout(poll, 2500);
    });
  }

  // 手机锁屏、切后台以后 SSE 可能悄悄断了却没报错：超过 50 秒没收到心跳就重连
  setInterval(function () {
    if (es && Date.now() - lastEvent > 50000) connect();
  }, 10000);

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) return;
    if (unread) { unread = 0; document.title = '文件传输助手'; }
    if (es && Date.now() - lastEvent > 25000) connect();
    // 手机锁屏、切后台回来：卡住的上传不等定时器，马上问清楚电脑收到多少，接着传
    for (var id in uploads) {
      var u = uploads[id];
      if (u.state === 'retry' || (u.state === 'sending' && Date.now() - u.lastTick > 3000)) restart(u, 0);
    }
  });

  // ---------------------------------------------------------------- 上传
  //
  // 每个文件按 16 MB 一块顺序发。断线以后不能直接从这块开头重发：电脑上可能已经有这块的
  // 前半截。所以先 GET /api/upload?id= 问清楚收到了多少，再从那里接着发。
  // u.attempt 是这个文件当前这次请求的编号，旧请求的回调对不上编号就什么都不做，
  // 保证同一个文件任何时候只有一条请求链。

  var queue = [], active = 0, sentTotal = 0, speed = 0, speedSamples = [], ticker = null;

  function slice(f, a, b) {
    var fn = f.slice || f.webkitSlice || f.mozSlice;
    return fn.call(f, a, b);
  }

  function pastedName(f) {
    var n = f.name || '';
    if (n && n !== 'image.png' && n !== 'image.jpg' && n !== 'blob') return n;
    var d = new Date(), ext = extOf(n) || (f.type && f.type.split('/')[1]) || 'png';
    return '截图 ' + d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
      pad(d.getHours()) + pad(d.getMinutes()) + pad(d.getSeconds()) + '.' + ext.replace('jpeg', 'jpg');
  }

  function addFiles(files) {
    if (!files || !files.length) return;
    var arr = [], i;
    for (i = 0; i < files.length; i++) arr.push(files[i]);
    for (i = 0; i < arr.length; i++) {
      var f = arr[i];
      var u = {
        id: rid(), file: f, name: pastedName(f), size: f.size || 0, type: f.type || '',
        sent: 0, cur: 0, tries: 0, c409: 0, state: 'queued', seq: seq++, speed: 0, samples: [], lastTick: 0, attempt: 0
      };
      uploads[u.id] = u;
      upsert({ id: u.id, pending: true, type: 'file', name: u.name, size: u.size, mime: u.type, fromId: me.id, from: me.name, time: Date.now(), seq: u.seq }, true);
      queue.push(u);
      wantThumb(u);
    }
    updateEmpty();
    toBottom();
    pump();
    startTicker();
  }

  function pump() {
    while (active < PARALLEL && queue.length) {
      var u = queue.shift();
      if (u.state !== 'queued') continue;
      u.slot = true;
      active++;
      if (u.needProbe) { // 手动重试的：先问清楚电脑上有多少
        u.needProbe = false;
        u.state = 'retry';
        probe(u);
      } else sendChunk(u);
    }
    paintTotals();
  }

  function release(u) {
    if (u.slot) { u.slot = false; active--; }
  }

  function sendChunk(u) {
    if (u.state === 'canceled' || u.state === 'done') return;
    u.state = 'sending';
    var my = ++u.attempt, start = u.sent, end = Math.min(u.size, start + CHUNK);
    var x = new XMLHttpRequest();
    u.xhr = x;
    x.open('POST', '/api/upload?id=' + u.id + '&size=' + u.size + '&offset=' + start + '&name=' + encodeURIComponent(u.name), true);
    x.setRequestHeader('X-LT', '1');
    x.setRequestHeader('X-Device-Id', me.id);
    x.setRequestHeader('X-Device-Name', encodeURIComponent(me.name));
    x.setRequestHeader('Content-Type', 'application/octet-stream');
    u.lastTick = Date.now();
    if (x.upload) {
      x.upload.onprogress = function (e) {
        if (my !== u.attempt) return;
        var c = start + e.loaded;
        if (c > u.cur) { sentTotal += c - u.cur; u.cur = c; }
        u.lastTick = Date.now();
      };
    }
    x.onload = function () {
      if (my !== u.attempt) return;
      u.xhr = null;
      var d = parseJSON(x.responseText) || {};
      if (x.status === 200 && d.done && d.item) { finishUpload(u, d.item); return; }
      if ((x.status === 200 || x.status === 409) && typeof d.received === 'number') {
        // 200：这块收好了；409：电脑上的进度和网页以为的不一样。都从电脑说的位置接着发
        if (x.status === 200) { u.tries = 0; u.c409 = 0; }
        else if (++u.c409 > 5) { failUpload(u, '电脑上的进度一直对不上，请点「重试」'); return; }
        setSent(u, Math.min(d.received, u.size));
        sendChunk(u);
        return;
      }
      if (x.status === 401) { failUpload(u, '需要重新输入访问码'); needLogin(); return; }
      if (x.status === 507) { failUpload(u, d.error || '电脑硬盘空间不够了'); return; }
      if (x.status >= 400 && x.status < 500 && x.status !== 408) { failUpload(u, d.error || '电脑拒绝了（' + x.status + '）'); return; }
      retry(u);
    };
    x.onerror = x.onabort = function () {
      if (my !== u.attempt) return;
      u.xhr = null;
      retry(u);
    };
    x.send(u.size ? slice(u.file, start, end) : null);
  }

  function setSent(u, n) {
    if (n > u.cur) sentTotal += n - u.cur;
    u.sent = u.cur = n;
  }

  // 连接断了：等一会儿（越试等得越久，最多 5 秒），然后先问进度再接着发
  function retry(u) {
    if (u.state !== 'sending' && u.state !== 'retry') return;
    u.tries++;
    if (u.tries > MAX_TRIES) { failUpload(u, '连不上电脑了：手机还连着同一个 Wi-Fi 吗？电脑上的程序还开着吗？'); return; }
    restart(u, Math.min(600 * u.tries, 5000));
  }

  // 作废正在进行的请求，delay 毫秒后问进度、接着传
  function restart(u, delay) {
    u.attempt++;
    if (u.xhr) { var x = u.xhr; u.xhr = null; try { x.abort(); } catch (e) {} }
    clearTimeout(u.retryTimer);
    u.state = 'retry';
    rerender(u.id);
    u.retryTimer = setTimeout(function () { probe(u); }, delay);
  }

  function probe(u) {
    if (u.state !== 'retry') return;
    var my = ++u.attempt;
    api('GET', '/api/upload?id=' + u.id, null, function (st, d) {
      if (my !== u.attempt || u.state !== 'retry') return;
      if (st === 200 && d.done && d.item) { finishUpload(u, d.item); return; }
      if (st === 200 && typeof d.received === 'number') {
        setSent(u, Math.min(d.received, u.size));
        sendChunk(u);
        return;
      }
      if (st === 401) { failUpload(u, '需要重新输入访问码'); return; }
      retry(u);
    });
  }

  function failUpload(u, msg) {
    u.attempt++;
    u.state = 'error';
    u.err = msg;
    release(u);
    rerender(u.id);
    pump();
  }

  function finishUpload(u, item) {
    u.attempt++;
    clearTimeout(u.retryTimer);
    u.state = 'done';
    release(u);
    u.cur = u.size;
    var it = items[u.id];
    if (it && it._preview) item._preview = it._preview;
    upsert(item);
    if (u.thumb) sendThumb(u);
    else if (!u.wantsThumb) delete uploads[u.id];
    if (!u.wantsThumb) u.file = null; // 缩略图还没做的话，等做完再放掉
    pump();
  }

  function cancelUpload(id) {
    var u = uploads[id];
    if (!u) return;
    u.state = 'canceled';
    u.attempt++;
    clearTimeout(u.retryTimer);
    if (u.xhr) { var x = u.xhr; u.xhr = null; try { x.abort(); } catch (e) {} }
    release(u);
    u.file = null;
    delete uploads[id];
    removeItem(id);
    api('POST', '/api/upload/cancel?id=' + id, null, null);
    pump();
  }

  function retryUpload(id) {
    var u = uploads[id];
    if (!u || u.state !== 'error') return;
    u.state = 'queued';
    u.tries = 0;
    u.c409 = 0;
    u.needProbe = true;
    queue.unshift(u);
    rerender(id);
    pump();
    startTicker();
  }

  function rerender(id) { if (items[id]) upsert(items[id], true); }

  function paintUpload(u) {
    var el = $('m-' + u.id);
    if (!el) return;
    var bar = el.querySelector('.progress i'), st = el.querySelector('.status');
    var pct = u.size ? (u.cur / u.size) * 100 : u.state === 'done' ? 100 : 0;
    if (bar) bar.style.width = pct.toFixed(1) + '%';
    if (!st) return;
    var small = !!el.querySelector('.ov'), s = '';
    if (u.state === 'queued') s = '等待发送 · ' + fmtSize(u.size);
    else if (u.state === 'sending') {
      var stuck = Date.now() - u.lastTick > 3000, fast = !stuck && u.speed >= 1024;
      s = Math.floor(pct) + '%';
      if (stuck) s += ' · 等待网络…';
      else if (fast) s += ' · ' + fmtSize(u.speed) + '/s';
      if (!small) s += ' · ' + fmtSize(u.cur) + ' / ' + fmtSize(u.size) + (fast ? ' · ' + fmtEta((u.size - u.cur) / u.speed) : '');
    } else if (u.state === 'retry') s = Math.floor(pct) + '% · 连接断了一下，正在接着传…';
    else if (u.state === 'error') s = '没发出去：' + (u.err || '网络断开了');
    else if (u.state === 'done') s = '已发送';
    setText(st, s);
    st.className = 'status' + (u.state === 'error' ? ' err' : '');
  }

  function paintTotals() {
    var n = 0, done = 0, total = 0, failed = 0, id, el = $('totals');
    for (id in uploads) {
      var u = uploads[id];
      if (u.state === 'error') failed++;
      if (u.state !== 'queued' && u.state !== 'sending' && u.state !== 'retry') continue;
      n++;
      total += u.size;
      done += u.cur;
    }
    if (!n) {
      el.hidden = true;
      return;
    }
    var pct = total ? (done / total) * 100 : 0;
    el.innerHTML = '正在发送 ' + n + ' 个文件 · ' + fmtSize(done) + ' / ' + fmtSize(total) +
      (speed >= 1024 ? ' · ' + fmtSize(speed) + '/s' : '') + (failed ? ' · ' + failed + ' 个失败' : '') +
      '<div class="progress"><i style="width:' + pct.toFixed(1) + '%"></i></div>' +
      (isTouch() ? '<div class="tot-hint">传完之前别锁屏、别切到别的 App，不然浏览器会暂停传输（回来后会自动接着传）</div>' : '');
    el.hidden = false;
  }

  function startTicker() {
    if (!ticker) ticker = setInterval(tick, 500);
  }

  // 速度按最近 3 秒实际传了多少来算：停住了就是 0，
  // 不会像指数平均那样慢慢衰减成「0.0002 B/s」这种数
  function rate(samples, now, value) {
    samples.push([now, value]);
    while (samples.length > 2 && now - samples[0][0] > 3000) samples.shift();
    var dt = (now - samples[0][0]) / 1000;
    return dt >= 0.9 ? Math.max(0, (value - samples[0][1]) / dt) : 0;
  }

  function tick() {
    var now = Date.now(), busy = false, id, u;
    speed = rate(speedSamples, now, sentTotal);
    for (id in uploads) {
      u = uploads[id];
      if (u.state === 'sending') {
        busy = true;
        u.speed = rate(u.samples, now, u.cur);
        // 这么久一点进展都没有：当这个连接死了，问清楚进度重来
        if (now - u.lastTick > STALL_MS) retry(u);
      } else {
        u.samples = [];
        u.speed = 0;
        if (u.state === 'queued' || u.state === 'retry') busy = true;
      }
      paintUpload(u);
    }
    paintTotals();
    if (!busy) {
      clearInterval(ticker);
      ticker = null;
      speed = 0;
      speedSamples = [];
    }
  }

  window.onbeforeunload = function () {
    for (var id in uploads) {
      var s = uploads[id].state;
      if (s === 'sending' || s === 'queued' || s === 'retry') return '还有文件没发完，离开的话会中断。';
    }
  };

  // ---------------------------------------------------------------- 缩略图（发送方的浏览器生成，电脑不用解码图片）

  var thumbQ = [], thumbBusy = false;

  function wantThumb(u) {
    var k = kindOf({ type: 'file', name: u.name });
    if (k !== 'image' && k !== 'video') {
      if (/^image\//.test(u.type) && u.size < 50e6) k = 'image';
      else return;
    }
    if (k === 'image' && u.size > 50e6) return;
    u.wantsThumb = k;
    thumbQ.push(u);
    nextThumb();
  }

  function nextThumb() {
    if (thumbBusy) return;
    var u = thumbQ.shift();
    if (!u) return;
    if (u.state === 'canceled' || !u.file) { nextThumb(); return; }
    thumbBusy = true;
    makeThumb(u.file, u.wantsThumb, function (res) {
      thumbBusy = false;
      u.wantsThumb = '';
      if (u.state === 'done') u.file = null;
      if (res && u.state !== 'canceled') {
        u.thumb = res;
        var it = items[u.id];
        if (it) {
          if (res.url) it._preview = res.url;
          if (res.w) { it.w = res.w; it.h = res.h; }
          if (res.dur) it.dur = res.dur;
          rerender(u.id);
        }
        if (u.state === 'done') sendThumb(u);
      } else if (u.state === 'done') delete uploads[u.id];
      nextThumb();
    });
  }

  function sendThumb(u) {
    var r = u.thumb;
    delete uploads[u.id];
    if (!r) return;
    api('POST', '/api/thumb?id=' + u.id + '&w=' + (r.w || 0) + '&h=' + (r.h || 0) + '&dur=' + (r.dur || 0), r.blob || null, null);
  }

  function makeThumb(file, kind, cb) {
    var URL_ = window.URL || window.webkitURL;
    var canvas = document.createElement('canvas');
    if (!URL_ || !URL_.createObjectURL || !canvas.getContext) { cb(null); return; }
    var src = URL_.createObjectURL(file), done = false, meta = {}, timer, v = null;
    function finish(ok) {
      if (done) return;
      done = true;
      clearTimeout(timer);
      // 马上放掉视频解码器，别让它在手机上跟上传抢读文件
      if (v) { try { v.removeAttribute('src'); v.load(); } catch (e) {} }
      try { URL_.revokeObjectURL(src); } catch (e) {}
      cb(ok && meta.w ? meta : null);
    }
    timer = setTimeout(function () { finish(true); }, 15000);
    function draw(el, w, h) {
      meta.w = w;
      meta.h = h;
      var s = Math.min(1, 480 / Math.max(w, h));
      canvas.width = Math.max(1, Math.round(w * s));
      canvas.height = Math.max(1, Math.round(h * s));
      try { canvas.getContext('2d').drawImage(el, 0, 0, canvas.width, canvas.height); } catch (e) { finish(true); return; }
      toJpeg(canvas, function (blob) {
        if (blob) { meta.blob = blob; meta.url = URL_.createObjectURL(blob); }
        finish(true);
      });
    }
    if (kind === 'image') {
      var img = new Image();
      img.onload = function () { draw(img, img.naturalWidth || img.width, img.naturalHeight || img.height); };
      img.onerror = function () { finish(false); };
      img.src = src;
      return;
    }
    var drawn = false;
    v = document.createElement('video');
    v.muted = true;
    v.setAttribute('muted', '');
    v.setAttribute('playsinline', '');
    v.setAttribute('webkit-playsinline', '');
    v.preload = 'metadata';
    function grab() {
      if (drawn || done) return;
      if (v.videoWidth) { drawn = true; draw(v, v.videoWidth, v.videoHeight); }
    }
    v.onloadedmetadata = function () {
      meta.dur = v.duration && isFinite(v.duration) ? Math.round(v.duration * 10) / 10 : 0;
      meta.w = v.videoWidth;
      meta.h = v.videoHeight;
      var t = Math.min(1, (v.duration || 0) / 3);
      try { v.currentTime = t > 0.05 ? t : 0.05; } catch (e) {}
      setTimeout(grab, 4000); // 有的浏览器不触发 seeked，等一会儿直接截
    };
    v.onseeked = grab;
    v.onerror = function () { finish(true); };
    v.src = src;
    try { v.load(); } catch (e) {}
  }

  function toJpeg(canvas, cb) {
    if (canvas.toBlob) {
      try { canvas.toBlob(cb, 'image/jpeg', 0.8); return; } catch (e) {}
    }
    try {
      var bin = atob(canvas.toDataURL('image/jpeg', 0.8).split(',')[1]), a = new Uint8Array(bin.length);
      for (var i = 0; i < bin.length; i++) a[i] = bin.charCodeAt(i);
      cb(new Blob([a], { type: 'image/jpeg' }));
    } catch (e) { cb(null); }
  }

  // ---------------------------------------------------------------- 文字

  var input = $('input'), sendBtn = $('btn-send'), composing = false;

  function autosize() {
    input.style.height = 'auto';
    input.style.height = Math.min(input.scrollHeight + 2, 160) + 'px';
  }

  function sendText() {
    var t = input.value;
    if (!/\S/.test(t)) return;
    input.value = '';
    autosize();
    sendBtn.disabled = true;
    var it = { id: rid(), type: 'text', text: t, pending: true, fromId: me.id, from: me.name, time: Date.now(), seq: seq++ };
    upsert(it);
    toBottom();
    postText(it);
  }

  function postText(it) {
    it.failed = false;
    rerender(it.id);
    api('POST', '/api/text', { id: it.id, text: it.text }, function (st, d) {
      if (st === 200 && d.id) { upsert(d); return; }
      var cur = items[it.id];
      if (cur && cur.pending) { cur.failed = true; rerender(it.id); }
      if (st !== 401) toast(d.error || '没发出去，点「重发」再试一次');
    });
  }

  function copyText(t, cb) {
    function fallback() {
      var ta = document.createElement('textarea'), ok = false;
      ta.value = t;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.top = '0';
      ta.style.left = '0';
      ta.style.opacity = '0';
      ta.style.fontSize = '16px';
      document.body.appendChild(ta);
      try {
        if (isIOS) {
          var r = document.createRange();
          r.selectNodeContents(ta);
          var sel = window.getSelection();
          sel.removeAllRanges();
          sel.addRange(r);
          ta.setSelectionRange(0, t.length);
        } else ta.select();
        ok = document.execCommand('copy');
      } catch (e) {}
      document.body.removeChild(ta);
      cb(ok);
    }
    // 剪贴板 API 只在 https 或 localhost 下可用，手机通过局域网 IP 打开时走老办法
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(t).then(function () { cb(true); }, fallback);
    } else fallback();
  }

  // ---------------------------------------------------------------- 点消息上的按钮

  function download(it) {
    if (inApp) toast('微信 / QQ 里下载不了文件，请点右上角「···」→「在浏览器打开」', 4500);
    var url = fileUrl(it, true), a = document.createElement('a');
    if ('download' in a) {
      a.href = url;
      a.setAttribute('download', it.name);
      a.style.display = 'none';
      document.body.appendChild(a);
      a.click();
      setTimeout(function () { if (a.parentNode) a.parentNode.removeChild(a); }, 1000);
    } else window.location.href = url;
    if (!inApp) toast('开始下载：' + it.name);
  }

  function hostOpen(it, reveal) {
    api('POST', '/api/host/open', { id: it.id, reveal: !!reveal }, function (st, d) {
      if (st !== 200) toast(d.error || '打不开');
      else if (!reveal && RISKY[extOf(it.name)]) toast('为了安全，程序和脚本只在文件夹里选中，不直接运行');
    });
  }

  function delItem(it, after) {
    var msg = it.type === 'file' ? '删除「' + it.name + '」？\n电脑接收文件夹里的这个文件也会一起删除。' : '删除这条消息？';
    if (!window.confirm(msg)) return;
    api('POST', '/api/delete', { ids: [it.id] }, function (st, d) {
      if (st === 200) { removeItem(it.id); if (after) after(); }
      else toast(d.error || '删除失败');
    });
  }

  function doAction(name, it) {
    var k = kindOf(it);
    switch (name) {
      case 'view':
        if (!it.pending) openViewer(it.id);
        break;
      case 'file':
        if (isHost) hostOpen(it, false);
        else if (k === 'audio') openViewer(it.id);
        else if (k === 'pdf') window.open(fileUrl(it), '_blank');
        else download(it);
        break;
      case 'dl': download(it); break;
      case 'open': hostOpen(it, false); break;
      case 'reveal': hostOpen(it, true); break;
      case 'copy':
        copyText(it.text, function (ok) { toast(ok ? '已复制' : '复制失败，请长按文字手动复制'); });
        break;
      case 'del': delItem(it); break;
      case 'cancel': cancelUpload(it.id); break;
      case 'retry': retryUpload(it.id); break;
      case 'resend': postText(it); break;
    }
  }

  function findAct(e, root) {
    var t = e.target, name = null;
    while (t && t !== root) {
      if (!name && t.getAttribute && t.getAttribute('data-act')) name = t.getAttribute('data-act');
      if (t.getAttribute && t.getAttribute('data-id') && /(^| )msg( |$)/.test(t.className)) return name ? { name: name, id: t.getAttribute('data-id') } : null;
      t = t.parentNode;
    }
    return null;
  }

  list.onclick = function (e) {
    var a = findAct(e, list);
    if (a && items[a.id]) doAction(a.name, items[a.id]);
  };
  list.onkeydown = function (e) {
    // 真正的 <button> 按回车本来就会触发 click，这里只管 role="button" 的卡片
    if ((e.keyCode !== 13 && e.keyCode !== 32) || e.target.tagName === 'BUTTON') return;
    var a = findAct(e, list);
    if (a && items[a.id]) { e.preventDefault(); doAction(a.name, items[a.id]); }
  };

  // ---------------------------------------------------------------- 大图、视频

  var vList = [], vIdx = 0, vOpen = false, vPushed = false;

  function openViewer(id) {
    var it = items[id];
    if (!it) return;
    vList = [];
    var nodes = list.childNodes;
    for (var i = 0; i < nodes.length; i++) {
      var o = items[nodes[i].getAttribute('data-id')];
      if (o && !o.pending && (kindOf(o) === 'image' || kindOf(o) === 'video')) vList.push(o);
    }
    vIdx = -1;
    for (i = 0; i < vList.length; i++) if (vList[i].id === id) vIdx = i;
    if (vIdx < 0) { vList = [it]; vIdx = 0; }
    if (!vOpen) {
      vOpen = true;
      $('viewer').hidden = false;
      // 安卓的返回键 / 手势：关掉查看器，而不是离开网页
      try { window.history.pushState({ lt: 'viewer' }, ''); vPushed = true; } catch (e) {}
    }
    showV();
  }

  function stopMedia() {
    var m = $('v-stage').querySelector('video,audio');
    if (m) {
      try { m.pause(); m.removeAttribute('src'); m.load(); } catch (e) {}
    }
  }

  function showV() {
    var it = vList[vIdx], k = kindOf(it), stage = $('v-stage');
    stopMedia();
    stage.innerHTML = '';
    setText($('v-name'), it.name);
    setText($('v-sub'), fmtSize(it.size) + ' · ' + (it.from || '') + ' · ' + fmtTime(it.time) + (vList.length > 1 ? ' · ' + (vIdx + 1) + ' / ' + vList.length : ''));
    var fail = function (msg) { return function () { stage.innerHTML = '<div class="v-msg">' + msg + '</div>'; }; };
    if (k === 'image') {
      var img = document.createElement('img');
      img.alt = '';
      img.onerror = fail('浏览器打不开这张图片（比如 HEIC 格式）<br>下载后用相册或看图软件打开');
      img.src = fileUrl(it);
      stage.appendChild(img);
    } else if (k === 'video') {
      var v = document.createElement('video');
      v.controls = true;
      v.autoplay = true;
      v.setAttribute('playsinline', '');
      v.setAttribute('webkit-playsinline', '');
      v.preload = 'auto';
      if (it.thumb) v.poster = thumbUrl(it);
      v.onerror = fail('这个视频的格式浏览器播不了<br>下载后用相册或播放器打开');
      v.src = fileUrl(it);
      stage.appendChild(v);
    } else if (k === 'audio') {
      var a = document.createElement('audio');
      a.controls = true;
      a.autoplay = true;
      a.onerror = fail('浏览器播不了这个音频，下载后再听');
      a.src = fileUrl(it);
      stage.appendChild(a);
    }
    if (vList.length > 1) {
      stage.insertAdjacentHTML('beforeend', '<button type="button" class="v-nav v-prev" data-v="prev" aria-label="上一个">‹</button><button type="button" class="v-nav v-next" data-v="next" aria-label="下一个">›</button>');
    }
    var b = '';
    if (isHost) b += '<button type="button" class="v-btn primary" data-v="open">用电脑打开</button><button type="button" class="v-btn" data-v="reveal">在文件夹中显示</button>';
    else b += '<button type="button" class="v-btn primary" data-v="dl">' + (k === 'image' ? '下载原图' : '下载') + '</button>';
    b += '<button type="button" class="v-btn" data-v="del">删除</button>';
    var hint = '';
    if (inApp) hint = '在微信 / QQ 里没法下载，请点右上角「···」→「在浏览器打开」';
    else if (!isHost && isIOS && k === 'image') hint = '存到相册：长按图片 →「添加到照片」';
    else if (!isHost && isIOS && k === 'video') hint = '存到相册：点「下载」，再到「文件」App 里打开它 → 分享 →「存储视频」';
    else if (!isHost && isAndroid) hint = '下载的文件在手机的「Download / 下载」文件夹，相册里也能找到';
    if (hint) b += '<div class="v-hint">' + esc(hint) + '</div>';
    $('v-bottom').innerHTML = b;
  }

  function navV(d) {
    if (vList.length < 2) return;
    vIdx = (vIdx + d + vList.length) % vList.length;
    showV();
  }

  function closeViewer(fromPop) {
    if (!vOpen) return;
    vOpen = false;
    stopMedia();
    $('v-stage').innerHTML = '';
    $('viewer').hidden = true;
    if (vPushed && fromPop !== true) {
      vPushed = false;
      try { window.history.back(); } catch (e) {}
    }
    vPushed = false;
  }

  window.onpopstate = function () { if (vOpen) closeViewer(true); };

  $('v-close').onclick = closeViewer;
  $('viewer').onclick = function (e) {
    var t = e.target, v = t.getAttribute && t.getAttribute('data-v');
    if (!v && t.parentNode && t.parentNode.getAttribute) v = t.parentNode.getAttribute('data-v');
    var it = vList[vIdx];
    if (!it) return;
    if (v === 'prev') navV(-1);
    else if (v === 'next') navV(1);
    else if (v === 'dl') download(it);
    else if (v === 'open') hostOpen(it, false);
    else if (v === 'reveal') hostOpen(it, true);
    else if (v === 'del') delItem(it, function () { closeViewer(); });
    else if (t === $('v-stage')) closeViewer(); // 点图片外面的空白处关闭
  };

  (function () {
    var sx = null, sy = 0, stage = $('v-stage');
    stage.addEventListener('touchstart', function (e) {
      if (e.touches.length !== 1) { sx = null; return; }
      sx = e.touches[0].clientX;
      sy = e.touches[0].clientY;
    }, false);
    stage.addEventListener('touchend', function (e) {
      if (sx === null || !e.changedTouches.length) return;
      var dx = e.changedTouches[0].clientX - sx, dy = e.changedTouches[0].clientY - sy;
      sx = null;
      if (Math.abs(dx) > 60 && Math.abs(dy) < 60 && kindOf(vList[vIdx] || {}) === 'image') navV(dx < 0 ? 1 : -1);
    }, false);
  })();

  // ---------------------------------------------------------------- 弹出面板：扫码、在线设备、设置

  var sheetKind = '';

  function openSheet(kind, html) {
    sheetKind = kind;
    $('sheet-body').innerHTML = '<button class="icon-btn close" type="button" data-sa="close" aria-label="关闭"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M6 6l12 12M18 6L6 18"/></svg></button>' + html;
    $('sheet').hidden = false;
  }

  function closeSheet() {
    sheetKind = '';
    $('sheet').hidden = true;
    $('sheet-body').innerHTML = '';
  }

  function showQR() { openSheet('qr', qrCardHTML(false)); }

  function showOnline() {
    var h = '<h3>在线设备</h3><ul class="dev-list">';
    for (var i = 0; i < online.length; i++) {
      var d = online[i];
      h += '<li>' + esc(d.name) + '<span class="tag">' + (d.id === me.id ? '本机' : d.host ? '运行程序的电脑' : '') + '</span></li>';
    }
    if (!online.length) h += '<li>还没连上电脑</li>';
    h += '</ul>';
    if (isHost) h += '<div class="row" style="margin-top:14px"><button type="button" class="btn primary" data-sa="show-qr">连接新设备</button></div>';
    openSheet('online', h);
  }

  function showSettings() {
    var h = '<h3>设置</h3>';
    h += '<div class="field"><label for="s-name">这台设备的名字</label><div class="row"><input class="inp" id="s-name" maxlength="24" value="' + esc(me.name) + '"><button class="btn" type="button" data-sa="save-name">保存</button></div><div class="hint">显示在你发出的消息旁边，方便分清是哪台设备发的。</div></div>';
    if (isHost && host) {
      h += '<div class="field"><span class="lbl">收到的文件保存在' + (host.free ? '（这个盘还剩 ' + fmtSize(host.free) + '）' : '') + '</span><div class="path">' + esc(host.dir) + '</div>';
      h += '<div class="row" style="margin-top:8px"><button class="btn" type="button" data-sa="open-dir">打开文件夹</button></div></div>';
      h += '<div class="field"><label for="s-dir">换一个文件夹</label><div class="row"><input class="inp" id="s-dir" placeholder="粘贴完整路径，比如 D:\\收到的文件"><button class="btn" type="button" data-sa="save-dir">更改</button></div></div>';
      h += '<div class="field"><span class="lbl">访问码</span><div class="row"><div class="path" style="flex:1">' + (host.noAuth ? '已关闭' : esc(host.code)) + '</div><button class="btn" type="button" data-sa="reset-code">换一个</button></div>';
      h += '<div class="hint">扫码进来不用输；手动输入地址时才要填。换了以后，已经连上的设备要重新扫码。</div>';
      h += '<label class="chk"><input type="checkbox" data-sc="noAuth"' + (host.noAuth ? ' checked' : '') + '> 不需要访问码（只建议在自己家里的 Wi-Fi 用）</label>';
      h += '<label class="chk"><input type="checkbox" data-sc="noBrowser"' + (host.noBrowser ? ' checked' : '') + '> 启动程序时不自动打开这个网页</label></div>';
      h += '<div class="field"><span class="lbl">聊天记录</span><label class="chk"><input type="checkbox" id="s-clear-files"> 同时删除收到的文件</label>';
      h += '<div class="row" style="margin-top:8px"><button class="btn danger" type="button" data-sa="clear">清空所有记录</button></div></div>';
    }
    h += '<div class="about">文件传输助手 ' + esc(version) + ' · <a href="https://github.com/haoawake/lan-transfer" target="_blank" rel="noopener noreferrer">GitHub</a></div>';
    openSheet('settings', h);
  }

  function hostSettings(body, okMsg) {
    api('POST', '/api/host/settings', body, function (st, d) {
      if (st === 200) { host = d; refreshHostUI(); if (okMsg) toast(okMsg); }
      else { toast(d.error || '保存失败', 4000); if (sheetKind === 'settings') showSettings(); }
    });
  }

  function sheetAction(name) {
    var u;
    switch (name) {
      case 'close': closeSheet(); break;
      case 'show-qr': showQR(); break;
      case 'settings': showSettings(); break;
      case 'qr-all':
        qrAll = true;
        $('empty')._html = '';
        refreshHostUI();
        break;
      case 'copy-link':
        u = host && host.urls && host.urls[qrIdx];
        if (u) copyText(u.link, function (ok) { toast(ok ? '链接已复制，发到手机上打开就行（链接里带着访问码，别发给别人）' : '复制失败', 4000); });
        break;
      case 'save-name':
        var n = $('s-name').value.replace(/^\s+|\s+$/g, '');
        if (!n) { toast('名字不能是空的'); return; }
        me.name = n.slice(0, 24);
        ls('lt.name', me.name);
        connect(); // 用新名字重新连接，其他设备马上能看到
        toast('已保存');
        break;
      case 'open-dir':
        api('POST', '/api/host/open', {}, function (st, d) { if (st !== 200) toast(d.error || '打不开'); });
        break;
      case 'save-dir':
        var dir = $('s-dir').value;
        if (!/\S/.test(dir)) { toast('先填一个文件夹路径'); return; }
        hostSettings({ dir: dir }, '已更改，以后收到的文件会存到新文件夹');
        break;
      case 'reset-code':
        if (!window.confirm('换一个新的访问码和二维码？\n已经连上的手机、平板需要重新扫码。')) return;
        api('POST', '/api/host/reset', {}, function (st, d) {
          if (st === 200) { host = d; qrVer++; refreshHostUI(); toast('已更换'); }
          else toast(d.error || '更换失败');
        });
        break;
      case 'clear':
        var files = $('s-clear-files') && $('s-clear-files').checked;
        if (!window.confirm(files ? '清空所有聊天记录，并删除收到的文件？\n删除的文件不能恢复。' : '清空所有聊天记录？\n收到的文件会留在文件夹里。')) return;
        api('POST', '/api/host/clear', { files: !!files }, function (st, d) {
          if (st === 200) { closeSheet(); toast('已清空'); }
          else toast(d.error || '清空失败');
        });
        break;
    }
  }

  // 扫码卡片既可能在空白页里，也可能在弹出面板里，所以在 document 上统一处理
  document.addEventListener('click', function (e) {
    var t = e.target;
    while (t && t !== document) {
      if (t.getAttribute) {
        var q = t.getAttribute('data-qr'), sa = t.getAttribute('data-sa');
        if (q !== null) {
          qrIdx = +q;
          $('empty')._html = '';
          refreshHostUI();
          return;
        }
        if (sa) { sheetAction(sa); return; }
      }
      t = t.parentNode;
    }
  }, false);

  $('sheet').onclick = function (e) { if (e.target === this) closeSheet(); };
  $('sheet').onchange = function (e) {
    var k = e.target.getAttribute && e.target.getAttribute('data-sc');
    if (!k) return;
    var body = {};
    body[k] = !!e.target.checked;
    hostSettings(body, '已保存');
  };

  $('btn-qr').onclick = showQR;
  $('online').onclick = showOnline;
  $('btn-menu').onclick = showSettings;
  $('btn-more').onclick = function () {
    var oldest = Infinity;
    for (var id in items) if (items.hasOwnProperty(id) && !items[id].pending && items[id].time < oldest) oldest = items[id].time;
    if (oldest === Infinity) return;
    api('GET', '/api/items?before=' + oldest + '&limit=100', null, function (st, d) {
      if (st !== 200) { toast('加载失败'); return; }
      var h0 = scroller.scrollHeight, top0 = scroller.scrollTop;
      for (var i = 0; i < d.items.length; i++) upsert(d.items[i], true);
      loadedOlder = true;
      hasMore = !!d.hasMore;
      $('more').hidden = !hasMore;
      scroller.scrollTop = top0 + (scroller.scrollHeight - h0);
    });
  };

  // ---------------------------------------------------------------- 输入访问码

  var loginShown = false;

  function needLogin() {
    if (loginShown) return;
    loginShown = true;
    if (es) { try { es.close(); } catch (e) {} es = null; }
    setConnected(false);
    $('login').hidden = false;
  }

  $('login-code').oninput = function () {
    var v = this.value.replace(/\D/g, '').slice(0, 6);
    if (v !== this.value) this.value = v;
    if (v.length === 6) submitLogin();
  };

  function submitLogin() {
    var code = $('login-code').value.replace(/\D/g, '');
    var err = $('login-err');
    if (code.length !== 6) { setText(err, '访问码是 6 位数字'); return; }
    setText(err, '');
    api('POST', '/api/login', { code: code }, function (st, d) {
      if (st === 200) {
        loginShown = false;
        $('login').hidden = true;
        $('login-code').value = '';
        start();
      } else {
        setText(err, st === 0 ? '连不上电脑，请检查手机和电脑是不是在同一个 Wi-Fi' : d.error || '出错了（' + st + '）');
        $('login-code').value = '';
      }
    });
  }

  $('login-form').onsubmit = function (e) {
    if (e.preventDefault) e.preventDefault();
    submitLogin();
    return false;
  };

  // ---------------------------------------------------------------- 输入框、选文件、拖放、粘贴

  input.oninput = function () {
    autosize();
    sendBtn.disabled = !/\S/.test(input.value);
  };
  input.addEventListener('compositionstart', function () { composing = true; });
  input.addEventListener('compositionend', function () { composing = false; });
  input.onkeydown = function (e) {
    // 回车发送，Shift + 回车换行；输入法正在选字时的回车不算
    if (e.keyCode === 13 && !e.shiftKey && !e.isComposing && !composing) {
      e.preventDefault();
      sendText();
    }
  };
  sendBtn.onmousedown = function (e) { e.preventDefault(); }; // 点发送时输入框不失焦，手机键盘不收起
  sendBtn.onclick = sendText;

  $('pick-media').onchange = $('pick-file').onchange = function () {
    addFiles(this.files);
    try { this.value = ''; } catch (e) {}
  };

  function hasFiles(e) {
    var t = e.dataTransfer && e.dataTransfer.types;
    if (!t) return false;
    for (var i = 0; i < t.length; i++) if (t[i] === 'Files') return true;
    return false;
  }

  var dragDepth = 0;
  document.addEventListener('dragenter', function (e) {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dragDepth++;
    $('drop').hidden = false;
  }, false);
  document.addEventListener('dragover', function (e) {
    if (!hasFiles(e)) return;
    e.preventDefault();
    try { e.dataTransfer.dropEffect = 'copy'; } catch (x) {}
  }, false);
  document.addEventListener('dragleave', function () {
    if (--dragDepth <= 0) { dragDepth = 0; $('drop').hidden = true; }
  }, false);
  document.addEventListener('drop', function (e) {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dragDepth = 0;
    $('drop').hidden = true;
    var dt = e.dataTransfer, files = [], dirs = 0, i;
    if (dt.items && dt.items.length && dt.items[0].webkitGetAsEntry) {
      for (i = 0; i < dt.items.length; i++) {
        var item = dt.items[i];
        if (item.kind !== 'file') continue;
        var entry = item.webkitGetAsEntry();
        if (entry && entry.isDirectory) { dirs++; continue; }
        var f = item.getAsFile();
        if (f) files.push(f);
      }
    } else {
      for (i = 0; i < dt.files.length; i++) files.push(dt.files[i]);
    }
    if (dirs) toast('暂时不支持直接发送文件夹，请先把它压缩成 zip 再拖进来', 4000);
    addFiles(files);
  }, false);

  document.addEventListener('paste', function (e) {
    var cd = e.clipboardData;
    if (!cd) return;
    var files = [], i;
    if (cd.files && cd.files.length) for (i = 0; i < cd.files.length; i++) files.push(cd.files[i]);
    else if (cd.items) {
      for (i = 0; i < cd.items.length; i++) {
        if (cd.items[i].kind === 'file') { var f = cd.items[i].getAsFile(); if (f) files.push(f); }
      }
    }
    if (!files.length) return;
    var text = '';
    try { text = cd.getData('text/plain'); } catch (x) {}
    if (text && document.activeElement === input) return; // 复制的是文字（比如从 Word 里），交给输入框
    e.preventDefault();
    addFiles(files);
  }, false);

  document.addEventListener('keydown', function (e) {
    if (vOpen) {
      if (e.keyCode === 27) closeViewer();
      else if (e.keyCode === 37) navV(-1);
      else if (e.keyCode === 39) navV(1);
    } else if (sheetKind && e.keyCode === 27) closeSheet();
  }, false);

  // ---------------------------------------------------------------- 启动

  function start() {
    api('GET', '/api/sync', null, function (st, d) {
      if (st === 401) return;
      if (st !== 200) { setConnected(false); setTimeout(start, 2000); return; }
      setConnected(true);
      applySync(d);
      connect();
    });
  }

  if (isTouch()) input.setAttribute('placeholder', '输入文字');
  function isTouch() { return 'ontouchstart' in window || navigator.maxTouchPoints > 0; }

  updateBanner();
  start();
})();
