// Copied verbatim from github.com/azimjohn/jprq at
// cli/debugger/static/tailwind.config.js, upstream commit 3c10e25,
// licensed under the MIT License (Copyright (c) 2020 Azimjon Pulatov).
/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ["./**/*.{html,js}"],
  theme: {
    extend: {
      colors: {
        jprq: {
          bg: '#f5f8ff',
          black: {
            40: '',
          }
        },
      },
    },
  },
  plugins: [],
}