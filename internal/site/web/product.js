const params = new URLSearchParams(window.location.search);
const source = params.get('source');
const id = params.get('id');
const status = document.querySelector('#product-status');
const detail = document.querySelector('#product-detail');
const fallback = document.querySelector('#product-fallback');
const image = document.querySelector('#product-image');
const placeholder = document.querySelector('#product-photo-placeholder');
const shopURL = 'https://www.etsy.com/shop/OllaEmporiumFR';

function showError(message) {
  status.textContent = message;
  fallback.hidden = false;
}

function safeImageURL(value) {
  if (typeof value !== 'string') return '';
  if (value.startsWith('/') && !value.startsWith('//')) return value;
  try {
    const url = new URL(value);
    return url.protocol === 'https:' ? url.href : '';
  } catch (_) {
    return '';
  }
}

function validEtsyURL(value) {
  try {
    const url = new URL(value);
    return url.protocol === 'https:' && (url.hostname === 'www.etsy.com' || url.hostname === 'etsy.com') && !url.username && !url.password;
  } catch (_) {
    return false;
  }
}

function showProduct(product, isDemo) {
  const title = document.querySelector('#product-title');
  const price = document.querySelector('#product-price');
  const action = document.querySelector('#product-action');
  const actionNote = document.querySelector('#product-action-note');
  title.textContent = product.title;
  document.title = product.title + ' | Olla Emporium';
  price.textContent = product.price || '';
  price.hidden = !product.price;
  document.querySelector('#product-description').textContent = product.description || 'Handmade with joy in the south of France.';
  document.querySelector('#product-source').textContent = source === 'etsy' ? 'FROM THE ETSY SHOP' : 'ONLY AT OLLA EMPORIUM';

  const src = safeImageURL(product.image_url);
  const showPlaceholder = () => { image.hidden = true; placeholder.hidden = false; };
  if (src) {
    image.alt = product.title;
    image.addEventListener('error', showPlaceholder, { once: true });
    image.src = src;
  } else {
    showPlaceholder();
  }

  if (source === 'etsy' && !isDemo && validEtsyURL(product.url)) {
    action.href = product.url;
    action.textContent = 'Buy on Etsy ↗';
    actionNote.textContent = 'Opens Etsy in a new tab. Choose your options and add this piece to your basket there.';
  } else if (source === 'etsy' && isDemo) {
    action.href = shopURL;
    action.textContent = 'Visit the Etsy shop ↗';
    actionNote.textContent = 'This is a demo piece, not an actual listing. The shop opens in a new tab.';
  } else if (source === 'site') {
    action.href = shopURL;
    action.textContent = 'Enquire on Etsy ↗';
    actionNote.textContent = 'This website-only piece cannot be purchased online here yet. Visit the shop in a new tab to enquire.';
  } else {
    throw new Error('Invalid Etsy listing URL');
  }
  detail.hidden = false;
  status.textContent = isDemo ? 'Preview only — this is an example, not a live Etsy listing.' : '';
}

if ((source !== 'etsy' && source !== 'site') || !id) {
  showError('We couldn’t find that piece.');
} else {
  fetch(source === 'etsy' ? '/api/featured-listings' : '/api/products')
    .then((response) => {
      if (!response.ok) throw new Error('Listings unavailable');
      return response.json();
    })
    .then((data) => {
      const listings = source === 'etsy' ? data.listings : data.products;
      if (!Array.isArray(listings) || (source === 'etsy' && !['mock', 'etsy'].includes(data.source))) throw new Error('Invalid response');
      const product = listings.find((listing) => listing.id === id);
      if (!product || typeof product.title !== 'string') {
        showError('This piece is no longer available to view.');
        return;
      }
      showProduct(product, source === 'etsy' && data.source === 'mock');
    })
    .catch(() => showError('This piece is unavailable right now. Please try again later.'));
}
