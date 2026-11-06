'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const webDirectory = path.join(__dirname, '..', 'internal', 'site', 'web');
const script = fs.readFileSync(path.join(webDirectory, 'script.js'), 'utf8');
const productScript = fs.readFileSync(path.join(webDirectory, 'product.js'), 'utf8');
const indexHTML = fs.readFileSync(path.join(webDirectory, 'index.html'), 'utf8');

class Element {
  constructor(tagName = 'div') {
    this.tagName = tagName;
    this.attributes = new Map();
    this.children = [];
    this.listeners = new Map();
    this.textContent = '';
    this.hidden = false;
    this.scrollWidth = 500;
    this.clientWidth = 200;
    this.scrollLeft = 0;
  }

  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  getAttribute(name) { return this.attributes.has(name) ? this.attributes.get(name) : null; }
  removeAttribute(name) { this.attributes.delete(name); }
  set href(value) { this.setAttribute('href', value); }
  get href() { return this.getAttribute('href'); }
  set src(value) { this.setAttribute('src', value); }
  get src() { return this.getAttribute('src'); }
  set alt(value) { this.setAttribute('alt', value); }
  get alt() { return this.getAttribute('alt'); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) {
    this.children = children;
    for (const child of children) child.parentElement = this;
  }
  get firstElementChild() { return this.children[0] || null; }
  get lastElementChild() { return this.children[this.children.length - 1] || null; }
  getBoundingClientRect() {
    if (this.tagName === 'ul') return { left: 0, right: this.clientWidth, width: this.clientWidth };
    if (this.tagName === 'li' && this.parentElement) {
      const left = this.parentElement.children.indexOf(this) * 120 - this.parentElement.scrollLeft;
      return { left, right: left + 100, width: 100 };
    }
    return { left: 0, right: 100, width: 100 };
  }
  scrollBy(options) { this.lastScroll = options; }
  before(element) { this.precedingElement = element; }
  querySelector(selector) {
    if (selector === 'img') return this.children.find((child) => child.tagName === 'img') || null;
    return this.selectors && this.selectors[selector] || null;
  }
  addEventListener(type, handler, options) {
    const listeners = this.listeners.get(type) || [];
    listeners.push({ handler, once: Boolean(options && options.once) });
    this.listeners.set(type, listeners);
  }
  dispatch(type, event = {}) {
    for (const listener of [...(this.listeners.get(type) || [])]) {
      if (listener.once) {
        this.listeners.set(type, this.listeners.get(type).filter((item) => item !== listener));
      }
      listener.handler(event);
    }
  }
}

function listing(id, title, image_url) { return { id, title, image_url }; }
const etsy = (listings, source = 'etsy') => ({ source, listings });
const site = (products) => ({ products });

function createPage({ etsyResponse = etsy([]), siteResponse = site([]), count = 5,
  random = () => 0, preload = 'success' } = {}) {
  const anchors = Array.from({ length: count }, (_, i) => {
    const anchor = new Element('a');
    const image = new Element('img');
    image.setAttribute('src', `/fallback-${i}.webp`);
    image.setAttribute('alt', `Fallback ${i}`);
    image.setAttribute('srcset', `/fallback-${i}-large.webp 2x`);
    anchor.append(image);
    return anchor;
  });
  const sections = {};
  const selectors = {};
  for (const source of ['etsy', 'site']) {
    const section = new Element('section');
    const controls = new Element();
    const previous = new Element('button');
    const next = new Element('button');
    const track = new Element('ul');
    const status = new Element('p');
    controls.selectors = { '.featured-previous': previous, '.featured-next': next };
    section.selectors = { '.featured-controls': controls };
    sections[source] = { section, controls, previous, next, track, status };
    selectors[`#${source}-featured`] = section;
    selectors[source === 'etsy' ? '#featured-track' : '#site-track'] = track;
    selectors[source === 'etsy' ? '#featured-status' : '#site-status'] = status;
  }
  const calls = [];
  const preloads = [];
  const document = {
    querySelector: (selector) => selectors[selector] || null,
    querySelectorAll: (selector) => selector === 'a[data-product-image]' ? anchors : [],
    createElement: (tag) => new Element(tag),
    addEventListener() {}
  };
  class PreloadImage {
    set src(value) {
      this.url = value;
      preloads.push(this);
      if (preload !== 'manual') {
        queueMicrotask(() => {
          const result = typeof preload === 'function' ? preload(value) : preload;
          if (result === 'failure') this.onerror();
          else this.onload();
        });
      }
    }
  }
  const math = Object.create(Math);
  math.random = random;
  const context = vm.createContext({
    document, Image: PreloadImage, URL, URLSearchParams, Math: math,
    window: { addEventListener() {} },
    getComputedStyle: () => ({ columnGap: '20px' }),
    fetch: (endpoint) => {
      calls.push(endpoint);
      const response = endpoint === '/api/featured-listings' ? etsyResponse : siteResponse;
      if (response instanceof Error) return Promise.reject(response);
      if (response === 'http-error') return Promise.resolve({ ok: false });
      if (response === 'json-error') return Promise.resolve({ ok: true, json: () => Promise.reject(new Error('Invalid JSON')) });
      return Promise.resolve({ ok: true, json: () => Promise.resolve(response) });
    }
  });
  vm.runInContext(script, context, { filename: 'script.js' });
  return { anchors, sections, calls, preloads, ready: vm.runInContext('editorialImagesReady', context) };
}

function values(anchor) {
  const image = anchor.querySelector('img');
  return { href: anchor.getAttribute('href'), src: image.getAttribute('src'),
    srcset: image.getAttribute('srcset'), alt: image.getAttribute('alt') };
}

function fallback(index) {
  return { href: null, src: `/fallback-${index}.webp`,
    srcset: `/fallback-${index}-large.webp 2x`, alt: `Fallback ${index}` };
}

test('shuffles both sources, avoids repeats, links the right products and keeps carousels working', async () => {
  const page = createPage({
    etsyResponse: etsy([listing('e&1', 'Etsy mug', 'https://images.example/mug.jpg'),
      listing('e2', 'Etsy plate', '/etsy-plate.webp')]),
    siteResponse: site([listing('s 1', 'Site bowl', '/product-images/bowl.webp')]),
    count: 7
  });
  await page.ready;
  assert.deepEqual(page.calls, ['/api/featured-listings', '/api/products']);
  const results = page.anchors.map(values);
  assert.deepEqual(results.slice(0, 3), [
    { href: '/product?source=etsy&id=e2', src: '/etsy-plate.webp', srcset: null, alt: 'Etsy plate' },
    { href: '/product?source=site&id=s+1', src: '/product-images/bowl.webp', srcset: null, alt: 'Site bowl' },
    { href: '/product?source=etsy&id=e%261', src: 'https://images.example/mug.jpg', srcset: null, alt: 'Etsy mug' }
  ]);
  assert.deepEqual(results.slice(3, 6), results.slice(0, 3));
  assert.deepEqual(results[6], results[0]);
  assert.equal(page.sections.etsy.track.children.length, 2);
  assert.equal(page.sections.site.track.children.length, 1);
  assert.equal(page.sections.etsy.track.hidden, false);
  assert.equal(page.sections.site.track.hidden, false);
  assert.equal(page.sections.etsy.controls.hidden, false);
  assert.equal(page.sections.etsy.previous.disabled, true);
  assert.equal(page.sections.etsy.next.disabled, false);
  page.sections.etsy.next.dispatch('click');
  assert.equal(page.sections.etsy.track.lastScroll.left, 120);
  page.sections.etsy.track.scrollLeft = 300;
  page.sections.etsy.track.dispatch('scroll');
  assert.equal(page.sections.etsy.next.disabled, true);
  assert.equal(page.sections.etsy.section.precedingElement, page.sections.site.section);
});

test('shows every returned active listing and opens a later listing on its detail page', async () => {
  const listings = Array.from({ length: 65 }, (_, i) => ({
    ...listing(String(i + 1), `Piece ${i + 1}`, `/piece-${i + 1}.webp`),
    description: `Description ${i + 1}`,
    url: `https://www.etsy.com/listing/${i + 1}`
  }));
  const page = createPage({ etsyResponse: etsy(listings), count: 65, random: () => 0.999 });
  await page.ready;
  assert.deepEqual(page.calls, ['/api/featured-listings', '/api/products']);
  assert.equal(page.sections.etsy.track.children.length, listings.length);
  assert.equal(page.sections.etsy.status.textContent, 'Explore all active listings from the Etsy shop.');
  assert.equal(page.sections.etsy.track.lastElementChild.children[0].href, '/product?source=etsy&id=65');
  assert.equal(values(page.anchors[64]).href, '/product?source=etsy&id=65');

  const elements = {};
  for (const id of ['status', 'detail', 'fallback', 'image', 'photo-placeholder', 'title', 'price', 'description', 'source', 'action', 'action-note']) {
    elements[`#product-${id}`] = new Element();
  }
  elements['#product-detail'].hidden = true;
  const calls = [];
  const document = { querySelector: (selector) => elements[selector] || null, title: '' };
  const context = vm.createContext({
    document, URL, URLSearchParams,
    window: { location: { search: '?source=etsy&id=65' } },
    fetch: (endpoint) => {
      calls.push(endpoint);
      return Promise.resolve({ ok: true, json: () => Promise.resolve(etsy(listings)) });
    }
  });
  vm.runInContext(productScript, context, { filename: 'product.js' });
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls, ['/api/featured-listings']);
  assert.equal(elements['#product-detail'].hidden, false);
  assert.equal(elements['#product-title'].textContent, 'Piece 65');
  assert.equal(elements['#product-description'].textContent, 'Description 65');
  assert.equal(elements['#product-action'].href, 'https://www.etsy.com/listing/65');
});

test('labels the Etsy carousel and its empty and unavailable states without featured wording', async () => {
  assert.match(indexHTML, /id="etsy-featured"/);
  assert.match(indexHTML, /aria-label="Active Etsy listings"/);
  assert.doesNotMatch(indexHTML, /aria-label="Featured (?:Etsy )?listings"/);
  const empty = createPage({ etsyResponse: etsy([]), count: 0 });
  const unavailable = createPage({ etsyResponse: 'http-error', count: 0 });
  await Promise.all([empty.ready, unavailable.ready]);
  assert.equal(empty.sections.etsy.status.textContent, 'There are no active Etsy listings right now. Visit the Etsy shop to see more.');
  assert.equal(unavailable.sections.etsy.status.textContent, 'Etsy listings are unavailable right now. Visit the Etsy shop to see the latest pieces.');
});

test('hides carousel controls when only trailing gutter is scrollable', async () => {
  const page = createPage({ etsyResponse: etsy([
    listing('1', 'One', '/1.webp'), listing('2', 'Two', '/2.webp'), listing('3', 'Three', '/3.webp')
  ]), count: 0 });
  await page.ready;
  const { track, controls, next } = page.sections.etsy;
  track.clientWidth = 340;
  track.scrollWidth = 344;
  track.dispatch('scroll');
  assert.equal(controls.hidden, true);
  track.clientWidth = 320;
  track.dispatch('scroll');
  assert.equal(controls.hidden, false);
  assert.equal(next.disabled, false);
});

test('random input changes the order without mutating API results', async () => {
  const listings = [listing('1', 'One', '/1.webp'), listing('2', 'Two', '/2.webp'),
    listing('3', 'Three', '/3.webp')];
  const first = createPage({ etsyResponse: etsy(listings), count: 3, random: () => 0 });
  const second = createPage({ etsyResponse: etsy(listings), count: 3, random: () => 0.999 });
  await Promise.all([first.ready, second.ready]);
  assert.deepEqual(first.anchors.map((a) => values(a).alt), ['Two', 'Three', 'One']);
  assert.deepEqual(second.anchors.map((a) => values(a).alt), ['One', 'Two', 'Three']);
  assert.deepEqual(listings.map((item) => item.id), ['1', '2', '3']);
});

test('ignores mock Etsy listings while preserving their carousel and using site products', async () => {
  const page = createPage({ etsyResponse: etsy([listing('demo', 'Demo', '/demo.webp')], 'mock'),
    siteResponse: site([listing('real', 'Real bowl', '/real.webp')]), count: 2 });
  await page.ready;
  assert.equal(page.sections.etsy.track.children.length, 1);
  assert.match(page.sections.etsy.status.textContent, /Preview only/);
  assert.equal(values(page.anchors[0]).href, '/product?source=site&id=real');
  assert.equal(values(page.anchors[1]).alt, 'Real bowl');
  assert.ok(page.preloads.every((image) => image.url !== '/demo.webp'));
});

test('leaves fallbacks untouched for mock-only, empty, malformed and unsafe products', async () => {
  const cases = [
    { etsyResponse: etsy([listing('demo', 'Demo', '/demo.webp')], 'mock') },
    { etsyResponse: etsy([], 'unknown'), siteResponse: site([]) },
    { siteResponse: site([null, listing('', 'No id', '/ok.webp'), listing(' ', 'No id', '/ok.webp'),
      listing('1', ' ', '/ok.webp'), listing('2', 'No image', ''),
      listing('3', 'HTTP', 'http://example.com/image.jpg'), listing('4', 'Protocol relative', '//example.com/a'),
      listing('5', 'Backslash', '/\\example.com/a'), listing('6', 'Controls', '/\n/example.com/a'),
      listing('7', 'Data URL', 'data:image/png;base64,abc'), listing('8', 'Credentials', 'https://user:pass@example.com/a')]) }
  ];
  for (const options of cases) {
    const page = createPage(options);
    await page.ready;
    assert.deepEqual(page.anchors.map(values), page.anchors.map((_, i) => fallback(i)));
    assert.equal(page.preloads.length, 0);
  }
});

test('deduplicates eligible products and still uses valid products when carousel rendering fails', async () => {
  const page = createPage({
    etsyResponse: etsy([null, listing('same', 'First', '/first.webp'), listing('same', 'Duplicate', '/duplicate.webp')]),
    siteResponse: site([listing('same', 'Other source', '/site.webp')]), count: 4
  });
  await page.ready;
  assert.match(page.sections.etsy.status.textContent, /unavailable/);
  assert.deepEqual(page.anchors.map((anchor) => values(anchor).alt), ['Other source', 'First', 'Other source', 'First']);
  assert.deepEqual(page.anchors.map((anchor) => values(anchor).href), [
    '/product?source=site&id=same', '/product?source=etsy&id=same',
    '/product?source=site&id=same', '/product?source=etsy&id=same'
  ]);
});

test('each API failure leaves the other source usable; both failing retain fallbacks', async () => {
  for (const failure of [new Error('Network error'), 'http-error', 'json-error', {}]) {
    const etsyDown = createPage({ etsyResponse: failure, siteResponse: site([listing('s', 'Site', '/site.webp')]), count: 1 });
    const siteDown = createPage({ etsyResponse: etsy([listing('e', 'Etsy', '/etsy.webp')]), siteResponse: failure, count: 1 });
    const bothDown = createPage({ etsyResponse: failure, siteResponse: failure, count: 1 });
    await Promise.all([etsyDown.ready, siteDown.ready, bothDown.ready]);
    assert.equal(values(etsyDown.anchors[0]).href, '/product?source=site&id=s');
    assert.equal(values(siteDown.anchors[0]).href, '/product?source=etsy&id=e');
    assert.deepEqual(values(bothDown.anchors[0]), fallback(0));
    assert.equal(etsyDown.calls.length, 2);
    assert.equal(siteDown.calls.length, 2);
    assert.match(etsyDown.sections.etsy.status.textContent, /unavailable/);
    assert.match(siteDown.sections.site.status.textContent, /unavailable/);
  }
});

test('preloads before swapping, and leaves each failed preload on its original fallback', async () => {
  const page = createPage({ etsyResponse: etsy([listing('e', 'Etsy', '/etsy.webp')]),
    siteResponse: site([listing('s', 'Site', '/site.webp')]), count: 2, preload: 'manual' });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(page.preloads.length, 2);
  assert.deepEqual(page.anchors.map(values), [fallback(0), fallback(1)]);
  page.preloads[0].onerror();
  page.preloads[1].onload();
  await page.ready;
  assert.deepEqual(values(page.anchors[0]), fallback(0));
  assert.equal(values(page.anchors[1]).href, '/product?source=etsy&id=e');
  assert.equal(values(page.anchors[1]).src, '/etsy.webp');
});

test('restores original image, alt and srcset and removes the link if the displayed image fails', async () => {
  const page = createPage({ siteResponse: site([listing('s', 'Site', '/site.webp')]), count: 1 });
  await page.ready;
  const anchor = page.anchors[0];
  assert.equal(values(anchor).href, '/product?source=site&id=s');
  anchor.querySelector('img').dispatch('error');
  assert.deepEqual(values(anchor), fallback(0));
  anchor.querySelector('img').dispatch('error');
  assert.deepEqual(values(anchor), fallback(0));
});
